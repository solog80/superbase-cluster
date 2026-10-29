package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrOutboxFull means the durable edge buffer has reached its configured size
// limit. Callers must fail the request so upstream agents keep their watermark.
var ErrOutboxFull = errors.New("outbox full")

var (
	outboxPendingBucket = []byte("pending")
	outboxDeadBucket    = []byte("dead")
	outboxDedupeBucket  = []byte("dedupe")
	outboxMetaBucket    = []byte("meta")
)

type outboxConfig struct {
	Path          string
	MaxBytes      int64
	MaxAttempts   int
	DedupeTTL     time.Duration
	DrainInterval time.Duration
	RetryBase     time.Duration
	RetryMax      time.Duration
}

// outboxJob is one durable unit of work. Payload is the raw request data, kept
// as JSON so the drain handler can rebuild it without the original HTTP request.
type outboxJob struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	DedupeKey     string          `json:"dedupe_key"`
	ReceivedAt    time.Time       `json:"received_at"`
	Payload       json.RawMessage `json:"payload"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error,omitempty"`
}

type outboxDedupeEntry struct {
	JobID     string    `json:"job_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type outboxStats struct {
	Enabled          bool           `json:"enabled"`
	Pending          int            `json:"pending"`
	Dead             int            `json:"dead"`
	OldestAgeSeconds int64          `json:"oldest_age_seconds"`
	Bytes            int64          `json:"bytes"`
	FileBytes        int64          `json:"file_bytes"`
	PendingByKind    map[string]int `json:"pending_by_kind,omitempty"`
	DeadByKind       map[string]int `json:"dead_by_kind,omitempty"`
	LastError        string         `json:"last_error,omitempty"`
}

type outbox struct {
	db  *bolt.DB
	cfg outboxConfig
}

// permanentJobError marks a job that cannot succeed on retry (for example a
// corrupt payload or unknown kind). It is moved straight to the dead bucket.
type permanentJobError struct {
	err error
}

func (e permanentJobError) Error() string { return e.err.Error() }
func (e permanentJobError) Unwrap() error { return e.err }

func permanentJob(err error) error {
	if err == nil {
		return nil
	}
	return permanentJobError{err: err}
}

func isPermanentJobError(err error) bool {
	var target permanentJobError
	return errors.As(err, &target)
}

func newOutbox(cfg outboxConfig) (*outbox, error) {
	if cfg.Path == "" {
		cfg.Path = "outbox.db"
	}
	if cfg.MaxAttempts < 0 {
		cfg.MaxAttempts = 0
	}
	if cfg.DedupeTTL <= 0 {
		cfg.DedupeTTL = 7 * 24 * time.Hour
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = 5 * time.Second
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 5 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 15 * time.Minute
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("create outbox directory: %w", err)
	}

	db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open outbox: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{
			outboxPendingBucket,
			outboxDeadBucket,
			outboxDedupeBucket,
			outboxMetaBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize outbox: %w", err)
	}

	return &outbox{db: db, cfg: cfg}, nil
}

func (o *outbox) Close() error {
	if o == nil || o.db == nil {
		return nil
	}
	return o.db.Close()
}

// Enqueue durably stores one job before returning. A non-empty dedupeKey that
// is still within its TTL prevents the same payload from being queued twice.
func (o *outbox) Enqueue(kind, dedupeKey string, payload any, receivedAt time.Time) (bool, error) {
	if o == nil || o.db == nil {
		return false, errors.New("outbox disabled")
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	receivedAt = receivedAt.UTC()

	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal job payload: %w", err)
	}
	if dedupeKey == "" {
		sum := sha256.Sum256(append([]byte(kind+"\x00"), body...))
		dedupeKey = hex.EncodeToString(sum[:])
	}

	var duplicate bool
	now := time.Now().UTC()
	err = o.db.Update(func(tx *bolt.Tx) error {
		dedupe := tx.Bucket(outboxDedupeBucket)
		if raw := dedupe.Get([]byte(dedupeKey)); raw != nil {
			var entry outboxDedupeEntry
			if json.Unmarshal(raw, &entry) == nil && entry.ExpiresAt.After(now) {
				duplicate = true
				return nil
			}
			if err := dedupe.Delete([]byte(dedupeKey)); err != nil {
				return err
			}
		}

		pending := tx.Bucket(outboxPendingBucket)
		seq, err := pending.NextSequence()
		if err != nil {
			return err
		}
		key := makeOutboxKey(receivedAt, seq)
		job := outboxJob{
			ID:            hex.EncodeToString(key),
			Kind:          kind,
			DedupeKey:     dedupeKey,
			ReceivedAt:    receivedAt,
			Payload:       body,
			NextAttemptAt: receivedAt,
		}
		encoded, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if o.cfg.MaxBytes > 0 && pendingBytes(tx)+int64(len(key)+len(encoded)) > o.cfg.MaxBytes {
			return ErrOutboxFull
		}
		if err := pending.Put(key, encoded); err != nil {
			return err
		}
		if err := addPendingBytes(tx, int64(len(key)+len(encoded))); err != nil {
			return err
		}

		entry, err := json.Marshal(outboxDedupeEntry{
			JobID:     job.ID,
			ExpiresAt: now.Add(o.cfg.DedupeTTL),
		})
		if err != nil {
			return err
		}
		return dedupe.Put([]byte(dedupeKey), entry)
	})
	if err != nil {
		return false, err
	}
	return duplicate, nil
}

// Run drains the outbox until ctx is cancelled. It applies a global backoff
// after a failed attempt so a downed TimescaleDB is not hammered for every job.
func (o *outbox) Run(ctx context.Context, handle func(context.Context, outboxJob) error) {
	if o == nil {
		return
	}
	nextCleanup := time.Now().Add(time.Hour)
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		processed, err := o.DrainOnce(ctx, handle)
		if err != nil {
			failures++
			delay := o.backoff(failures)
			log.Printf("[Outbox] drain failed (attempt %d, next try in %s): %v", failures, delay, err)
			if !sleepContext(ctx, delay) {
				return
			}
		} else {
			if processed {
				failures = 0
			} else if !sleepContext(ctx, o.cfg.DrainInterval) {
				return
			}
		}

		if time.Now().After(nextCleanup) {
			o.cleanupDedupe()
			nextCleanup = time.Now().Add(time.Hour)
		}
	}
}

// DrainOnce processes at most one due job. It returns processed=true when a job
// was attempted, and a non-nil error when that attempt should trigger a global
// backoff.
func (o *outbox) DrainOnce(ctx context.Context, handle func(context.Context, outboxJob) error) (bool, error) {
	key, job, err := o.firstDue(time.Now().UTC())
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}

	handleErr := handle(ctx, *job)
	if handleErr == nil {
		if err := o.markDone(key); err != nil {
			return true, err
		}
		return true, nil
	}

	if isPermanentJobError(handleErr) {
		job.Attempts++
		if err := o.markDead(key, *job, handleErr); err != nil {
			return true, err
		}
		return true, nil
	}

	dead, err := o.recordFailure(key, *job, handleErr)
	if err != nil {
		return true, err
	}
	if dead {
		return true, nil
	}
	return true, handleErr
}

func (o *outbox) firstDue(now time.Time) ([]byte, *outboxJob, error) {
	var priorityKey, fallbackKey []byte
	var priorityJob, fallbackJob *outboxJob
	err := o.db.View(func(tx *bolt.Tx) error {
		pending := tx.Bucket(outboxPendingBucket)
		if pending == nil {
			return nil
		}
		c := pending.Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var job outboxJob
			if err := json.Unmarshal(raw, &job); err != nil {
				return fmt.Errorf("corrupt pending job: %w", err)
			}
			if job.NextAttemptAt.After(now) {
				continue
			}
			if fallbackJob == nil {
				fallbackKey = append([]byte(nil), k...)
				fallbackJob = &job
			}
			// Billing traffic jumps ahead of analytics snapshots, which can
			// otherwise delay TV/radio logs for many minutes after an outage.
			if isPriorityOutboxKind(job.Kind) {
				priorityKey = append([]byte(nil), k...)
				priorityJob = &job
				break
			}
		}
		return nil
	})
	if priorityJob != nil {
		return priorityKey, priorityJob, nil
	}
	return fallbackKey, fallbackJob, err
}

func isPriorityOutboxKind(kind string) bool {
	return kind == outboxKindVideoIngest || kind == outboxKindRadioIngest
}

func (o *outbox) markDone(key []byte) error {
	return o.db.Update(func(tx *bolt.Tx) error {
		pending := tx.Bucket(outboxPendingBucket)
		if raw := pending.Get(key); raw != nil {
			if err := addPendingBytes(tx, -int64(len(key)+len(raw))); err != nil {
				return err
			}
		}
		return pending.Delete(key)
	})
}

func (o *outbox) recordFailure(key []byte, job outboxJob, cause error) (bool, error) {
	job.Attempts++
	job.LastError = cause.Error()
	if o.cfg.MaxAttempts > 0 && job.Attempts >= o.cfg.MaxAttempts {
		return true, o.markDead(key, job, cause)
	}
	job.NextAttemptAt = time.Now().UTC().Add(o.backoff(job.Attempts))

	err := o.db.Update(func(tx *bolt.Tx) error {
		encoded, err := json.Marshal(job)
		if err != nil {
			return err
		}
		pending := tx.Bucket(outboxPendingBucket)
		if old := pending.Get(key); old != nil {
			if err := addPendingBytes(tx, int64(len(encoded)-len(old))); err != nil {
				return err
			}
		}
		if err := pending.Put(key, encoded); err != nil {
			return err
		}
		return o.setLastError(tx, cause)
	})
	return false, err
}

func (o *outbox) markDead(key []byte, job outboxJob, cause error) error {
	job.LastError = cause.Error()
	job.NextAttemptAt = time.Time{}
	return o.db.Update(func(tx *bolt.Tx) error {
		encoded, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if err := tx.Bucket(outboxDeadBucket).Put(key, encoded); err != nil {
			return err
		}
		pending := tx.Bucket(outboxPendingBucket)
		if raw := pending.Get(key); raw != nil {
			if err := addPendingBytes(tx, -int64(len(key)+len(raw))); err != nil {
				return err
			}
		}
		if err := pending.Delete(key); err != nil {
			return err
		}
		if job.DedupeKey != "" {
			if err := tx.Bucket(outboxDedupeBucket).Delete([]byte(job.DedupeKey)); err != nil {
				return err
			}
		}
		if err := o.setLastError(tx, cause); err != nil {
			return err
		}
		log.Printf("[Outbox] job %s moved to dead letter after %d attempt(s): %v", job.ID, job.Attempts, cause)
		return nil
	})
}

func (o *outbox) setLastError(tx *bolt.Tx, cause error) error {
	meta := tx.Bucket(outboxMetaBucket)
	if meta == nil {
		return nil
	}
	if err := meta.Put([]byte("last_error"), []byte(cause.Error())); err != nil {
		return err
	}
	return meta.Put([]byte("last_error_at"), []byte(time.Now().UTC().Format(time.RFC3339)))
}

func (o *outbox) Stats() outboxStats {
	stats := outboxStats{Enabled: true}
	if o == nil || o.db == nil {
		return stats
	}
	_ = o.db.View(func(tx *bolt.Tx) error {
		if pending := tx.Bucket(outboxPendingBucket); pending != nil {
			stats.Pending = pending.Stats().KeyN
			stats.PendingByKind = map[string]int{}
			c := pending.Cursor()
			for k, raw := c.First(); k != nil; k, raw = c.Next() {
				var job outboxJob
				if json.Unmarshal(raw, &job) != nil {
					continue
				}
				stats.PendingByKind[job.Kind]++
				if stats.OldestAgeSeconds == 0 {
					stats.OldestAgeSeconds = int64(time.Since(job.ReceivedAt).Seconds())
				}
			}
		}
		if dead := tx.Bucket(outboxDeadBucket); dead != nil {
			stats.Dead = dead.Stats().KeyN
			stats.DeadByKind = map[string]int{}
			c := dead.Cursor()
			for k, raw := c.First(); k != nil; k, raw = c.Next() {
				var job outboxJob
				if json.Unmarshal(raw, &job) == nil {
					stats.DeadByKind[job.Kind]++
				}
			}
		}
		stats.Bytes = pendingBytes(tx)
		if meta := tx.Bucket(outboxMetaBucket); meta != nil {
			stats.LastError = string(meta.Get([]byte("last_error")))
		}
		return nil
	})
	if fi, err := os.Stat(o.cfg.Path); err == nil {
		stats.FileBytes = fi.Size()
	}
	return stats
}

func pendingBytes(tx *bolt.Tx) int64 {
	meta := tx.Bucket(outboxMetaBucket)
	if meta == nil {
		return 0
	}
	raw := meta.Get([]byte("pending_bytes"))
	if raw == nil {
		return 0
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func addPendingBytes(tx *bolt.Tx, delta int64) error {
	meta := tx.Bucket(outboxMetaBucket)
	if meta == nil {
		return nil
	}
	value := pendingBytes(tx) + delta
	if value < 0 {
		value = 0
	}
	return meta.Put([]byte("pending_bytes"), []byte(strconv.FormatInt(value, 10)))
}

// ReplayDead moves up to limit dead-lettered jobs back to pending. It is used
// after a handler version upgrade (for example, when an older binary did not
// recognize a newly queued job kind).
func (o *outbox) ReplayDead(limit int) (int, error) {
	if o == nil || o.db == nil {
		return 0, errors.New("outbox disabled")
	}
	if limit <= 0 {
		limit = 1000
	}
	now := time.Now().UTC()
	moved := 0
	kinds := map[string]int{}
	err := o.db.Update(func(tx *bolt.Tx) error {
		dead := tx.Bucket(outboxDeadBucket)
		pending := tx.Bucket(outboxPendingBucket)
		dedupe := tx.Bucket(outboxDedupeBucket)

		type deadItem struct {
			key []byte
			raw []byte
		}
		items := make([]deadItem, 0, limit)
		c := dead.Cursor()
		for k, raw := c.First(); k != nil && len(items) < limit; k, raw = c.Next() {
			items = append(items, deadItem{
				key: append([]byte(nil), k...),
				raw: append([]byte(nil), raw...),
			})
		}

		for _, item := range items {
			var job outboxJob
			if err := json.Unmarshal(item.raw, &job); err != nil {
				continue
			}
			job.Attempts = 0
			job.LastError = ""
			job.NextAttemptAt = now
			encoded, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := pending.Put(item.key, encoded); err != nil {
				return err
			}
			if err := dead.Delete(item.key); err != nil {
				return err
			}
			if err := addPendingBytes(tx, int64(len(item.key)+len(encoded))); err != nil {
				return err
			}
			if job.DedupeKey != "" {
				entry, _ := json.Marshal(outboxDedupeEntry{
					JobID: job.ID, ExpiresAt: now.Add(o.cfg.DedupeTTL),
				})
				if err := dedupe.Put([]byte(job.DedupeKey), entry); err != nil {
					return err
				}
			}
			kinds[job.Kind]++
			moved++
		}
		return nil
	})
	if moved > 0 {
		log.Printf("[Outbox] replayed %d dead job(s): %v", moved, kinds)
	}
	return moved, err
}

func (o *outbox) cleanupDedupe() {
	if o == nil || o.db == nil {
		return
	}
	now := time.Now().UTC()
	_ = o.db.Update(func(tx *bolt.Tx) error {
		dedupe := tx.Bucket(outboxDedupeBucket)
		if dedupe == nil {
			return nil
		}
		var expired [][]byte
		c := dedupe.Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var entry outboxDedupeEntry
			if json.Unmarshal(raw, &entry) != nil || !entry.ExpiresAt.After(now) {
				expired = append(expired, append([]byte(nil), k...))
			}
		}
		for _, key := range expired {
			if err := dedupe.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o *outbox) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := o.cfg.RetryBase
	for i := 1; i < attempt; i++ {
		if delay >= o.cfg.RetryMax/2 {
			return o.cfg.RetryMax
		}
		delay *= 2
	}
	if delay > o.cfg.RetryMax {
		return o.cfg.RetryMax
	}
	return delay
}

func makeOutboxKey(receivedAt time.Time, seq uint64) []byte {
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(receivedAt.UnixNano()))
	binary.BigEndian.PutUint64(key[8:], seq)
	return key
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func outboxConfigFromEnv() outboxConfig {
	return outboxConfig{
		Path:          getenv("OUTBOX_PATH", "/var/lib/salt-gofn/outbox.db"),
		MaxBytes:      getenvInt64("OUTBOX_MAX_BYTES", 5<<30),
		MaxAttempts:   int(getenvInt64("OUTBOX_MAX_ATTEMPTS", 0)),
		DedupeTTL:     time.Duration(getenvInt64("OUTBOX_DEDUPE_TTL_HOURS", 168)) * time.Hour,
		DrainInterval: getenvDuration("OUTBOX_DRAIN_INTERVAL", 5*time.Second),
		RetryBase:     getenvDuration("OUTBOX_RETRY_BASE", 5*time.Second),
		RetryMax:      getenvDuration("OUTBOX_RETRY_MAX", 15*time.Minute),
	}
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return parsed
}

func getenvInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return parsed
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return parsed
}
