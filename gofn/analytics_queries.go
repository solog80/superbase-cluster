package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// handleGetFirebaseAnalytics mirrors firebaseAnalytics.js (User & Device page):
// aggregates content_sessions via the TimescaleDB get_firebase_analytics RPC
// (BigQuery replacement). Cached in-memory (metricsCacheTTL) so the dashboard
// polls don't re-scan the session hypertable.
func (s *server) handleGetFirebaseAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pStart := orNil(q.Get("startDate"))
	pEnd := orNil(q.Get("endDate"))
	if pStart == nil {
		pStart = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	}
	if pEnd == nil {
		pEnd = time.Now().UTC().Format("2006-01-02")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	payload, err := s.radioRPCPayload(ctx, "get_firebase_analytics", []string{"p_start", "p_end"},
		map[string]any{"p_start": pStart, "p_end": pEnd}, metricsCacheTTL)
	if err != nil {
		if errors.Is(err, errTsdbUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

// handleGetAnalyticsMetrics mirrors analyticsQueries.js getAnalyticsMetrics:
// aggregates content analytics via the TimescaleDB get_analytics_metrics RPC
// (BigQuery replacement). Returns the exact dashboard payload shape. Cached
// in-memory (metricsCacheTTL) so dashboard polls don't re-scan millions of rows.
func (s *server) handleGetAnalyticsMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	payload, err := s.radioRPCPayload(ctx, "get_analytics_metrics", nil, nil, metricsCacheTTL)
	if err != nil {
		if errors.Is(err, errTsdbUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}
