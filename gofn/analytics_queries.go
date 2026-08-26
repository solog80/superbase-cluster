package main

import (
	"context"
	"net/http"
	"time"
)

// handleGetAnalyticsMetrics mirrors analyticsQueries.js getAnalyticsMetrics:
// aggregates content analytics via the TimescaleDB get_analytics_metrics RPC
// (BigQuery replacement). Returns the exact dashboard payload shape.
func (s *server) handleGetAnalyticsMetrics(w http.ResponseWriter, r *http.Request) {
	db, err := s.tsdbDB(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var payload []byte
	err = db.QueryRowContext(ctx,
		`SELECT payload::text FROM public.get_analytics_metrics()`).Scan(&payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}
