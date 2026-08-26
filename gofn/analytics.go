package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Content analytics WRITERS — mirrors functions/src/analytics.js
// (batchTrackContentViews/Impressions/WatchProgress/Sessions). The Flutter app
// buffers events (Hive) and POSTs batches to these public endpoints. Rows land
// in TimescaleDB hypertables (BigQuery replacement). No auth (like the source).

type trackReq struct {
	Events []map[string]any `json:"events"`
}

// eventsTime converts a payload value to a timestamptz. The Flutter SDK sends
// ms-epoch ints; tolerates ISO strings and seconds-epoch floats too.
func eventsTime(v any) time.Time {
	switch t := v.(type) {
	case float64:
		if t > 1e12 {
			return time.UnixMilli(int64(t))
		}
		return time.Unix(int64(t), 0)
	case int64:
		return time.UnixMilli(t)
	case int:
		return time.UnixMilli(int64(t))
	case string:
		if t == "" {
			return time.Now().UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed
		}
	}
	return time.Now().UTC()
}

// collectCommon builds the shared device/geo/timestamp columns present in every
// analytics event type.
func collectCommon(ev map[string]any, now time.Time) map[string]any {
	return map[string]any{
		"user_id":      ev["user_id"],
		"profile_id":   ev["profile_id"],
		"country_code": ev["country_code"],
		"device_id":    ev["device_id"],
		"device_type":  ev["device_type"],
		"browser":      ev["browser"],
		"os":           ev["os"],
		"timestamp":    eventsTime(ev["timestamp"]),
		"received_at":  now,
	}
}

func (s *server) trackView(w http.ResponseWriter, r *http.Request) {
	s.ingestBatch(w, r, "content_views", func(ev map[string]any, common map[string]any) map[string]any {
		out := map[string]any{
			"content_id":               ev["content_id"],
			"content_type":             ev["content_type"],
			"content_name":             ev["content_name"],
			"creator_id":               ev["creator_id"],
			"program_duration_seconds": ev["program_duration_seconds"],
		}
		for k, v := range common {
			out[k] = v
		}
		return out
	})
}

func (s *server) trackImpression(w http.ResponseWriter, r *http.Request) {
	s.ingestBatch(w, r, "content_impressions", func(ev map[string]any, common map[string]any) map[string]any {
		out := map[string]any{
			"content_id":     ev["content_id"],
			"content_type":   ev["content_type"],
			"content_name":   ev["content_name"],
			"impression_type": ev["impression_type"],
		}
		for k, v := range common {
			out[k] = v
		}
		return out
	})
}

func (s *server) trackWatchProgress(w http.ResponseWriter, r *http.Request) {
	s.ingestBatch(w, r, "watch_progress", func(ev map[string]any, common map[string]any) map[string]any {
		out := map[string]any{
			"content_id":               ev["content_id"],
			"content_type":             ev["content_type"],
			"creator_id":               ev["creator_id"],
			"position":                 ev["position"],
			"duration":                 ev["duration"],
			"delta":                    ev["delta"],
			"program_duration_seconds": ev["program_duration_seconds"],
		}
		for k, v := range common {
			out[k] = v
		}
		return out
	})
}

func (s *server) trackContentSession(w http.ResponseWriter, r *http.Request) {
	s.ingestBatch(w, r, "content_sessions", func(ev map[string]any, common map[string]any) map[string]any {
		out := map[string]any{
			"content_id":               ev["content_id"],
			"content_type":             ev["content_type"],
			"session_start_time":       eventsTime(ev["session_start_time"]),
			"total_watch_time_seconds": ev["total_watch_time_seconds"],
			"end_reason":               ev["end_reason"],
			"program_duration_seconds": ev["program_duration_seconds"],
		}
		for k, v := range common {
			out[k] = v
		}
		return out
	})
}

// ingestBatch decodes { events: [...] }, normalizes each via build, and inserts
// into the given TSDB hypertable. Column map keys are the snake_case columns.
func (s *server) ingestBatch(w http.ResponseWriter, r *http.Request, table string, build func(map[string]any, map[string]any) map[string]any) {
	db, err := s.tsdbDB(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	var body trackReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if len(body.Events) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid payload: events array required"})
		return
	}

	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// Build the INSERT from the first row's keys (all rows share a shape).
	rows := make([]map[string]any, 0, len(body.Events))
	for _, ev := range body.Events {
		rows = append(rows, build(ev, collectCommon(ev, now)))
	}

	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}

	// INSERT ... (cols) VALUES (...), (...) — single statement, batched.
	placeholders, args := insertPlaceholders(rows, cols)
	stmt := "INSERT INTO public." + table + " (" + joinCols(cols) + ") VALUES " + placeholders
	if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
		log.Printf("%s insert error: %v", table, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	log.Printf("%s: inserted %d rows into timescale", table, len(body.Events))
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(body.Events),
	})
}

// insertPlaceholders returns a VALUES clause like ($1,$2),($3,$4) and the
// flattened arg list, walking rows in column order.
func insertPlaceholders(rows []map[string]any, cols []string) (string, []any) {
	var clauses []string
	var args []any
	idx := 1
	for _, row := range rows {
		var parts []string
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("$%d", idx))
			args = append(args, row[c])
			idx++
		}
		clauses = append(clauses, "("+strings.Join(parts, ",")+")")
	}
	return strings.Join(clauses, ","), args
}

func joinCols(cols []string) string {
	return strings.Join(cols, ",")
}
