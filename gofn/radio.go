package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// allowedRadioTables — the TSDB hypertables the Firestore sync jobs mirror to.
var allowedRadioTables = map[string]bool{
	"radio_history":        true,
	"radio_listeners":      true,
	"radio_nowplaying":     true,
	"radio_daily":          true,
	"radio_hourly":         true,
	"radio_best_worst":     true,
	"radio_country":        true,
	"radio_browser":        true,
	"radio_client":         true,
	"radio_stream":         true,
	"radio_listening_time": true,
}

// Radio analytics READERS — mirrors functions/src/azuracast.js getRadio*.
// Each calls a TimescaleDB RPC that returns the exact JSON payload the admin
// dashboard expects (BigQuery + Firestore EPG replaced).

// rpcCacheEntry holds a cached RPC payload with expiry.
type rpcCacheEntry struct {
	payload   []byte
	expiresAt time.Time
}

// rpcCacheTTL — the radio readers aggregate per-minute nowplaying data, so a
// 60s cache is plenty fresh while turning ~25s aggregations into sub-ms hits.
const rpcCacheTTL = 60 * time.Second

// metricsCacheTTL — get_analytics_metrics() scans millions of rows (content
// views / watch progress / sessions). Cache it longer so the analytics
// dashboard's first paint and 30s polls are fast.
const metricsCacheTTL = 300 * time.Second

// callRadioRPCRaw runs an RPC and returns the raw payload bytes, or nil if an
// error response was already written. Results are cached in-memory (keyed by
// RPC + args) so the slow radio aggregations don't re-run on every dashboard
// poll.
var errTsdbUnavailable = errors.New("timescale unavailable")

func (s *server) callRadioRPCRaw(w http.ResponseWriter, r *http.Request, rpc string, order []string, args map[string]any) []byte {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	payload, err := s.radioRPCPayload(ctx, rpc, order, args, rpcCacheTTL)
	if err != nil {
		if errors.Is(err, errTsdbUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return nil
	}
	return payload
}

// radioRPCPayload runs a TimescaleDB analytics RPC and returns its cached JSON
// payload bytes. Shared by the single-reader handlers, the aggregated
// getAdminAnalytics handler (which tolerates per-RPC failures), and the
// scheduler's dashboard pre-warm. ttl controls the in-memory cache lifetime.
func (s *server) radioRPCPayload(ctx context.Context, rpc string, order []string, args map[string]any, ttl time.Duration) ([]byte, error) {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTsdbUnavailable, err)
	}
	key := rpcCacheKey(rpc, order, args)

	s.rpcMu.Lock()
	if e, ok := s.rpcCache[key]; ok && time.Now().Before(e.expiresAt) {
		s.rpcMu.Unlock()
		return e.payload, nil
	}
	s.rpcMu.Unlock()

	query := "SELECT payload::text FROM public." + rpc + "("
	params := []any{}
	i := 1
	first := true
	for _, col := range order {
		if v, ok := args[col]; ok {
			if !first {
				query += ", "
			}
			query += "$" + strconv.Itoa(i)
			params = append(params, v)
			i++
			first = false
		}
	}
	query += ")"

	var payload []byte
	if err := db.QueryRowContext(ctx, query, params...).Scan(&payload); err != nil {
		return nil, err
	}

	s.rpcMu.Lock()
	s.rpcCache[key] = rpcCacheEntry{payload: payload, expiresAt: time.Now().Add(ttl)}
	s.rpcMu.Unlock()
	return payload, nil
}

// rpcCacheKey builds a stable key from the RPC name + its args (in column
// order, so position matches the function signature).
func rpcCacheKey(rpc string, order []string, args map[string]any) string {
	var b strings.Builder
	b.WriteString(rpc)
	for _, col := range order {
		if v, ok := args[col]; ok {
			fmt.Fprintf(&b, "|%s=%v", col, v)
		}
	}
	return b.String()
}

func (s *server) callRadioRPC(w http.ResponseWriter, r *http.Request, rpc string, order []string, args map[string]any) {
	payload := s.callRadioRPCRaw(w, r, rpc, order, args)
	if payload == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

func (s *server) handleGetRadioHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"p_days":     atoiDefault(q.Get("days"), 7),
		"p_limit":    atoiDefault(q.Get("limit"), 100),
		"p_offset":   atoiDefault(q.Get("offset"), 0),
		"p_search":   orNil(q.Get("search")),
		"p_sort":     orNil(q.Get("sortBy")),
		"p_sort_dir": orNil(q.Get("sortDir")),
	}
	s.callRadioRPC(w, r, "get_radio_history", []string{"p_days", "p_limit", "p_offset", "p_search", "p_sort", "p_sort_dir"}, args)
}

func (s *server) handleGetRadioReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"p_days":  atoiDefault(q.Get("days"), 30),
		"p_start": orNil(q.Get("startDate")),
		"p_end":   orNil(q.Get("endDate")),
	}
	payload := s.callRadioRPCRaw(w, r, "get_radio_reports", []string{"p_days", "p_start", "p_end"}, args)
	if payload == nil {
		return // already written
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad payload: " + err.Error()})
		return
	}

	// Live nowplaying from AzuraCast (the dashboard's "Now Playing" card).
	if cur, ok := s.fetchLiveNowPlaying(r.Context()); ok {
		out["current"] = cur
	}

	writeJSON(w, http.StatusOK, out)
}

// fetchLiveNowPlaying pulls the live AzuraCast /nowplaying for Salt FM and
// shapes it exactly like the original getRadioReports `current` object.
func (s *server) fetchLiveNowPlaying(ctx context.Context) (map[string]any, bool) {
	var np struct {
		Listeners struct {
			Total   int `json:"total"`
			Unique  int `json:"unique"`
			Current int `json:"current"`
		} `json:"listeners"`
		Station struct {
			Mounts []struct {
				Listeners struct {
					Current int `json:"current"`
				} `json:"listeners"`
			} `json:"mounts"`
			HlsListeners int `json:"hls_listeners"`
		} `json:"station"`
		NowPlaying struct {
			Song struct {
				Title  string `json:"title"`
				Artist string `json:"artist"`
				Text   string `json:"text"`
			} `json:"song"`
		} `json:"now_playing"`
		Live struct {
			IsLive       bool   `json:"is_live"`
			StreamerName string `json:"streamer_name"`
		} `json:"live"`
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.azuracastGet(ctx2, "/api/station/307/nowplaying", &np); err != nil {
		return nil, false
	}
	mount := 0
	if len(np.Station.Mounts) > 0 {
		mount = np.Station.Mounts[0].Listeners.Current
	}
	return map[string]any{
		"listeners_total":   np.Listeners.Total,
		"listeners_unique":  np.Listeners.Unique,
		"listeners_current": np.Listeners.Current,
		"hls_listeners":     np.Station.HlsListeners,
		"song_title":        np.NowPlaying.Song.Title,
		"song_artist":       np.NowPlaying.Song.Artist,
		"song_text":         np.NowPlaying.Song.Text,
		"streamer_name":     np.Live.StreamerName,
		"is_live":           np.Live.IsLive,
		"mount_current":     mount,
	}, true
}

func (s *server) handleGetRadioCountryDetails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"p_country": orNil(q.Get("country")),
		"p_limit":   atoiDefault(q.Get("limit"), 20),
	}
	s.callRadioRPC(w, r, "get_radio_country_details", []string{"p_country", "p_limit"}, args)
}

func (s *server) handleGetRadioShowAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"p_days":  atoiDefault(q.Get("days"), 30),
		"p_start": orNil(q.Get("startDate")),
		"p_end":   orNil(q.Get("endDate")),
	}
	s.callRadioRPC(w, r, "get_radio_show_analytics", []string{"p_days", "p_start", "p_end"}, args)
}

func (s *server) handleGetRadioShowSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var pLimit any
	if n := atoiDefault(q.Get("limit"), 0); n > 0 {
		pLimit = n
	}
	args := map[string]any{
		"p_start":  orNil(q.Get("startDate")),
		"p_end":    orNil(q.Get("endDate")),
		"p_limit":  pLimit,
		"p_offset": atoiDefault(q.Get("offset"), 0),
		"p_show":   orNil(q.Get("showName")),
	}
	s.callRadioRPC(w, r, "get_radio_show_snapshots", []string{"p_start", "p_end", "p_limit", "p_offset", "p_show"}, args)
}

func (s *server) handleGetRadioShowListenerDetails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"p_start":     orNil(q.Get("startDate")),
		"p_end":       orNil(q.Get("endDate")),
		"p_show":      orNil(q.Get("showName")),
		"p_page":      atoiDefault(q.Get("page"), 1),
		"p_page_size": atoiDefault(q.Get("pageSize"), 25),
	}
	s.callRadioRPC(w, r, "get_radio_show_listener_details", []string{"p_start", "p_end", "p_show", "p_page", "p_page_size"}, args)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
