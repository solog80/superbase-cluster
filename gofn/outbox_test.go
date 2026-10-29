package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testOutbox(t *testing.T, cfg outboxConfig) *outbox {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "outbox.db")
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = time.Millisecond
	}
	if cfg.RetryMax == 0 {
		cfg.RetryMax = 5 * time.Millisecond
	}
	ob, err := newOutbox(cfg)
	if err != nil {
		t.Fatalf("newOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	return ob
}

func TestOutboxEnqueueDedupeAndDrain(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 3, DedupeTTL: time.Hour})

	duplicate, err := ob.Enqueue("test", "same-key", map[string]string{"a": "b"}, time.Now())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if duplicate {
		t.Fatal("first enqueue reported duplicate")
	}

	duplicate, err = ob.Enqueue("test", "same-key", map[string]string{"a": "b"}, time.Now())
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if !duplicate {
		t.Fatal("second enqueue was not deduplicated")
	}
	stats := ob.Stats()
	if stats.Pending != 1 || stats.PendingByKind["test"] != 1 {
		t.Fatalf("stats = %+v, want pending=1 test", stats)
	}

	var handled int
	processed, err := ob.DrainOnce(context.Background(), func(_ context.Context, job outboxJob) error {
		handled++
		if job.Kind != "test" {
			t.Fatalf("kind = %q", job.Kind)
		}
		return nil
	})
	if err != nil || !processed {
		t.Fatalf("DrainOnce = (%v, %v), want (true, nil)", processed, err)
	}
	if handled != 1 {
		t.Fatalf("handled = %d, want 1", handled)
	}
	stats = ob.Stats()
	if stats.Pending != 0 || stats.Dead != 0 {
		t.Fatalf("stats after drain = %+v, want pending=0 dead=0", stats)
	}
}

func TestOutboxPrioritizesTrafficKinds(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	if _, err := ob.Enqueue(outboxKindAnalyticsRows, "analytics", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue analytics: %v", err)
	}
	if _, err := ob.Enqueue(outboxKindVideoIngest, "video", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue video: %v", err)
	}

	var order []string
	for i := 0; i < 2; i++ {
		if _, err := ob.DrainOnce(context.Background(), func(_ context.Context, job outboxJob) error {
			order = append(order, job.Kind)
			return nil
		}); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
	}
	if len(order) != 2 || order[0] != outboxKindVideoIngest || order[1] != outboxKindAnalyticsRows {
		t.Fatalf("order = %v, want video first", order)
	}
}

func TestOutboxRetriesThenDeadLetters(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 2, DedupeTTL: time.Hour})

	if _, err := ob.Enqueue("test", "retry", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fail := func(_ context.Context, _ outboxJob) error { return errors.New("tsdb down") }

	processed, err := ob.DrainOnce(context.Background(), fail)
	if err == nil || !processed {
		t.Fatalf("first DrainOnce = (%v, %v), want (true, error)", processed, err)
	}
	stats := ob.Stats()
	if stats.Pending != 1 || stats.Dead != 0 {
		t.Fatalf("after first failure = %+v", stats)
	}

	time.Sleep(10 * time.Millisecond)
	processed, err = ob.DrainOnce(context.Background(), fail)
	if err != nil || !processed {
		t.Fatalf("second DrainOnce = (%v, %v), want (true, nil) after dead-letter", processed, err)
	}
	stats = ob.Stats()
	if stats.Pending != 0 || stats.Dead != 1 {
		t.Fatalf("after dead-letter = %+v, want pending=0 dead=1", stats)
	}
	if stats.LastError == "" {
		t.Fatal("last error was not recorded")
	}

	duplicate, err := ob.Enqueue("test", "retry", map[string]string{"a": "b"}, time.Now())
	if err != nil {
		t.Fatalf("re-enqueue after dead-letter: %v", err)
	}
	if duplicate {
		t.Fatal("dead-lettered job must be re-enqueueable for manual recovery")
	}
}

func TestOutboxRetriesUnlimitedByDefault(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	if _, err := ob.Enqueue("test", "long-outage", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fail := func(_ context.Context, _ outboxJob) error { return errors.New("qnap down") }
	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Millisecond)
		if _, err := ob.DrainOnce(context.Background(), fail); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", i+1)
		}
	}
	stats := ob.Stats()
	if stats.Pending != 1 || stats.Dead != 0 {
		t.Fatalf("stats = %+v, want pending=1 dead=0 (retry until QNAP returns)", stats)
	}
}

func TestOutboxPermanentErrorAvoidsRetries(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 20, DedupeTTL: time.Hour})

	if _, err := ob.Enqueue("test", "poison", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	processed, err := ob.DrainOnce(context.Background(), func(_ context.Context, _ outboxJob) error {
		return permanentJob(errors.New("bad payload"))
	})
	if err != nil || !processed {
		t.Fatalf("DrainOnce = (%v, %v)", processed, err)
	}
	stats := ob.Stats()
	if stats.Pending != 0 || stats.Dead != 1 {
		t.Fatalf("stats = %+v, want pending=0 dead=1", stats)
	}
}

func TestOutboxReplayDead(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	if _, err := ob.Enqueue("test", "replay", map[string]string{"a": "b"}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := ob.DrainOnce(context.Background(), func(_ context.Context, _ outboxJob) error {
		return permanentJob(errors.New("bad payload"))
	}); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats := ob.Stats(); stats.Dead != 1 || stats.Pending != 0 {
		t.Fatalf("after dead-letter = %+v", stats)
	}

	moved, err := ob.ReplayDead(10)
	if err != nil {
		t.Fatalf("ReplayDead: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	if stats := ob.Stats(); stats.Dead != 0 || stats.Pending != 1 {
		t.Fatalf("after replay = %+v", stats)
	}
	if _, err := ob.DrainOnce(context.Background(), func(_ context.Context, _ outboxJob) error {
		return nil
	}); err != nil {
		t.Fatalf("replayed DrainOnce: %v", err)
	}
}

func TestOutboxFull(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxBytes: 1, MaxAttempts: 3, DedupeTTL: time.Hour})

	if _, err := ob.Enqueue("test", "full", map[string]string{"a": "b"}, time.Now()); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("enqueue error = %v, want ErrOutboxFull", err)
	}
}

func TestOutboxNotDueJobIsSkipped(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 3, DedupeTTL: time.Hour})

	if _, err := ob.Enqueue("test", "future", map[string]string{"a": "b"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	processed, err := ob.DrainOnce(context.Background(), func(_ context.Context, _ outboxJob) error {
		t.Fatal("handler must not run for a future job")
		return nil
	})
	if err != nil || processed {
		t.Fatalf("DrainOnce = (%v, %v), want (false, nil)", processed, err)
	}
}
