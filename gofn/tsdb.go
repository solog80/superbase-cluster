package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// tsdbConn wraps a connection to the dedicated TimescaleDB analytics instance
// (ug/QNAP, tailnet-only). Replaces BigQuery for ad-event ingestion + queries.
//
// The connection is created lazily and re-establishes itself if the underlying
// DB becomes unreachable (e.g. QNAP power loss) — the service no longer needs
// a restart to recover; callers use conn() and get either a live DB or an
// error explaining why analytics is unavailable.
type tsdbConn struct {
	mu  sync.Mutex
	dsn string
	db  *sql.DB
}

// newTSDB builds a connection config from env. Unlike the old behavior it does
// NOT fail at startup if the DB is unreachable — it records the DSN and lets
// conn() establish (and re-establish) the real connection on demand.
//
//	TSDB_DSN          postgres://user:pass@host:port/db   (overrides the rest)
//	TSDB_HOST         default 100.116.185.70 (ug/QNAP tailnet)
//	TSDB_PORT         default 55439
//	TSDB_DB           default analytics
//	TSDB_USER         default postgres
//	TSDB_PASSWORD / TIMESCALE_PASSWORD
func newTSDB() (*tsdbConn, error) {
	dsn := os.Getenv("TSDB_DSN")
	if dsn == "" {
		host := getenv("TSDB_HOST", "100.116.185.70")
		port := getenv("TSDB_PORT", "55439")
		dbname := getenv("TSDB_DB", "analytics")
		user := getenv("TSDB_USER", "postgres")
		pass := os.Getenv("TSDB_PASSWORD")
		if pass == "" {
			pass = os.Getenv("TIMESCALE_PASSWORD")
		}
		if pass == "" {
			return nil, fmt.Errorf("TSDB_PASSWORD/TIMESCALE_PASSWORD not set")
		}
		u := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(user, pass),
			Host:     host + ":" + port,
			Path:     "/" + dbname,
			RawQuery: "sslmode=disable",
		}
		dsn = u.String()
	}

	// Lazily attempt an initial connection so callers that need TSDB fail fast
	// (they get an error) but the process never dies because QNAP was down at
	// boot. Return the config even if the first dial fails.
	t := &tsdbConn{dsn: dsn}
	return t, nil
}

// conn returns a live *sql.DB, (re)connecting if the current one is missing or
// dead. Callers should treat a returned error as "analytics unavailable".
func (t *tsdbConn) conn(ctx context.Context) (*sql.DB, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.db != nil {
		// Fast path: assume the pool is healthy. database/sql re-dials on
		// demand, so a dead peer surfaces as a query error, not a nil DB.
		return t.db, nil
	}

	db, err := sql.Open("pgx", t.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, err
	}
	t.db = db
	return db, nil
}

// isUp reports whether a live connection exists (does not dial).
func (t *tsdbConn) isUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.db != nil
}

// db returns a live *sql.DB for server-side callers, reconnecting on demand.
func (s *server) tsdbDB(ctx context.Context) (*sql.DB, error) {
	if s.tsdb == nil {
		return nil, fmt.Errorf("analytics store not configured")
	}
	return s.tsdb.conn(ctx)
}

func (t *tsdbConn) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.db != nil {
		t.db.Close()
		t.db = nil
	}
}
