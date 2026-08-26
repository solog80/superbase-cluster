package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// adsCacheEntry holds a placement-filtered ad list with a 1h TTL (mirrors the
// old Upstash TTL of 3600s, now in-process).
type adsCacheEntry struct {
	ads       []map[string]any
	expiresAt time.Time
}

const adCacheTTL = time.Hour

// handleGetAdMobile mirrors ads.js getAdMobile: returns active ads, optionally
// filtered by placement. Reads via the public ads_api view (RLS-scoped to
// active), with a per-placement in-memory cache.
func (s *server) handleGetAdMobile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Placement string `json:"placement"`
		UserID    string `json:"userId"`
	}
	switch r.Method {
	case http.MethodGet:
		body.Placement = r.URL.Query().Get("placement")
	default:
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}

	cacheKey := body.Placement
	if cacheKey == "" {
		cacheKey = "all"
	}

	s.adsMu.Lock()
	if e, ok := s.adsCache[cacheKey]; ok && time.Now().Before(e.expiresAt) {
		s.adsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"source":  "cache",
			"ads":     e.ads,
		})
		return
	}
	s.adsMu.Unlock()

	vals := url.Values{
		"select": {"*"},
		"status": {"eq.active"},
		"order":  {"priority.desc"},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	raw, _, err := s.doRest(ctx, "ads_api", vals)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error(), "ads": []any{}})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "bad response: " + err.Error(), "ads": []any{}})
		return
	}

	var ads []map[string]any
	if body.Placement != "" {
		for _, ad := range rows {
			if placementMatches(ad, body.Placement) {
				ads = append(ads, ad)
			}
		}
	} else {
		ads = rows
	}
	if ads == nil {
		ads = []map[string]any{}
	}

	s.adsMu.Lock()
	s.adsCache[body.Placement] = adsCacheEntry{ads: ads, expiresAt: time.Now().Add(adCacheTTL)}
	if body.Placement != "" {
		s.adsCache["all"] = adsCacheEntry{ads: rows, expiresAt: time.Now().Add(adCacheTTL)}
	}
	s.adsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"source":  "db",
		"ads":     ads,
	})
}

// placementMatches checks the ad's placementType array for the requested
// placement (Firestore-style: ad.placementType.includes(placement)).
func placementMatches(ad map[string]any, placement string) bool {
	raw, ok := ad["placementType"]
	if !ok {
		return false
	}
	list, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, p := range list {
		if s, ok := p.(string); ok && s == placement {
			return true
		}
	}
	return false
}

// handleRefreshAdCache mirrors ads.js refreshAdCache: rebuilds the in-memory
// cache for all placements. Admin-gated (service role key).
func (s *server) handleRefreshAdCache(w http.ResponseWriter, r *http.Request) {
	s.adsMu.Lock()
	s.adsCache = map[string]adsCacheEntry{}
	s.adsMu.Unlock()

	vals := url.Values{
		"select": {"*"},
		"status": {"eq.active"},
		"order":  {"priority.desc"},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	raw, _, err := s.doRest(ctx, "ads_api", vals)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var ads []map[string]any
	_ = json.Unmarshal(raw, &ads)
	if ads == nil {
		ads = []map[string]any{}
	}

	now := time.Now().Add(adCacheTTL)
	s.adsMu.Lock()
	s.adsCache["all"] = adsCacheEntry{ads: ads, expiresAt: now}
	for _, placement := range []string{"pre-roll", "mid-roll", "banner"} {
		var filtered []map[string]any
		for _, ad := range ads {
			if placementMatches(ad, placement) {
				filtered = append(filtered, ad)
			}
		}
		s.adsCache[placement] = adsCacheEntry{ads: filtered, expiresAt: now}
	}
	s.adsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"message":    "Cache refreshed successfully",
		"adsCount":   len(ads),
		"placements": 4,
	})
}

// handleBatchTrackAdEvents mirrors ads.js batchTrackAdEvents: ingests ad
// events into the TimescaleDB ad_events hypertable (replaces BigQuery +
// Firestore fallback).
func (s *server) handleBatchTrackAdEvents(w http.ResponseWriter, r *http.Request) {
	db, err := s.tsdbDB(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid body: " + err.Error()})
		return
	}
	if len(body.Events) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "No events provided"})
		return
	}

	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	stmt := `INSERT INTO public.ad_events (ad_id, event_type, timestamp, user_id, watch_time, metadata)
	         VALUES ($1, $2, $3, $4, $5, $6)`
	for _, ev := range body.Events {
		ts, ok := parseEventTime(ev["timestamp"])
		if !ok {
			ts = now
		}
		var watchTime any
		if wv, ok := ev["watch_time"]; ok && wv != nil {
			watchTime = wv
		}
		var metadata any
		if m, ok := ev["metadata"]; ok && m != nil {
			metadata = m
		}
		userID := firstNonEmpty(ev["user_id"], "anonymous")
		if _, err := db.ExecContext(ctx, stmt,
			ev["ad_id"], ev["event_type"], ts, userID, watchTime, metadata); err != nil {
			log.Printf("ad_events insert error: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
			return
		}
	}

	log.Printf("ad_events: inserted %d rows into timescale", len(body.Events))
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"eventsProcessed": len(body.Events),
	})
}

// parseEventTime accepts an RFC3339 ISO string or a Firestore-style seconds
// value and returns a time.Time (ok=false on anything else → caller uses now).
func parseEventTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		if t == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	case float64:
		// Firestore Timestamp {_seconds} serialized as seconds epoch.
		if t > 1e12 {
			return time.UnixMilli(int64(t)), true
		}
		return time.Unix(int64(t), 0), true
	}
	return time.Time{}, false
}

// handleGetAdAnalytics mirrors ads.js getAdAnalytics: aggregates ad_events on
// TimescaleDB via the get_ad_analytics RPC (replaces BigQuery).
func (s *server) handleGetAdAnalytics(w http.ResponseWriter, r *http.Request) {
	db, err := s.tsdbDB(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var body struct {
		StartDate string `json:"startDate"`
		EndDate   string `json:"endDate"`
		AdID      string `json:"adId"`
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		body.StartDate, body.EndDate, body.AdID = q.Get("startDate"), q.Get("endDate"), q.Get("adId")
	default:
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}
	if body.StartDate == "" || body.EndDate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "startDate and endDate are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var payload []byte
	err = db.QueryRowContext(ctx,
		`SELECT payload::text FROM public.get_ad_analytics($1, $2, $3)`,
		body.StartDate, body.EndDate, nilOrEmpty(body.AdID),
	).Scan(&payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": err.Error(),
			"totalImpressions": 0, "totalClicks": 0, "avgCTR": 0, "avgCompletionRate": 0,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

// nilOrEmpty returns nil for "" so Postgres sees NULL (unfiltered).
func nilOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// handleScheduleAdCacheRefresh mirrors ads.js scheduleAdCacheRefresh (hourly
// pubsub). In the mesh, the in-memory TTL self-heals; this endpoint just
// warms/rebuilds the cache and is meant to be called by pg_cron.
func (s *server) handleScheduleAdCacheRefresh(w http.ResponseWriter, r *http.Request) {
	s.handleRefreshAdCache(w, r)
}

// firstNonEmpty returns v when it's a non-empty string, else def.
func firstNonEmpty(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}
