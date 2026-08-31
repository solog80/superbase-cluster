package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// handleGetAdminAnalytics aggregates the full analytics dashboard payload
// server-side, replacing the old src/app/api/analytics/route.ts (which fanned
// out to the Supabase Edge Function gateway + three GCP Cloud Run radio
// endpoints and merged in Next.js). It reads get_analytics_metrics() plus the
// radio RPCs straight from TimescaleDB and reproduces the merge exactly, so the
// analytics page and all its sub-pages keep the same response shape. Requires
// the service-role key (admin-only).
func (s *server) handleGetAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := atoiDefault(q.Get("days"), 30)
	var pStart, pEnd any
	if v := strings.TrimSpace(q.Get("startDate")); v != "" {
		pStart = v
	}
	if v := strings.TrimSpace(q.Get("endDate")); v != "" {
		pEnd = v
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	// 1. App analytics (TV/VOD/...): cached get_analytics_metrics() RPC.
	payload, err := s.radioRPCPayload(ctx, "get_analytics_metrics", nil, nil, metricsCacheTTL)
	if err != nil {
		if errors.Is(err, errTsdbUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "analytics: " + err.Error()})
		}
		return
	}
	merged := map[string]any{}
	if err := json.Unmarshal(payload, &merged); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad analytics payload: " + err.Error()})
		return
	}
	merged["days"] = days

	// 2. Radio data — best-effort (mirrors Promise.allSettled): any radio RPC
	// failure keeps the app analytics and just omits radio fields.
	radioArgs := map[string]any{"p_days": days, "p_start": pStart, "p_end": pEnd}
	radioData := s.radioPayloadFor(ctx, "get_radio_reports", []string{"p_days", "p_start", "p_end"}, radioArgs)
	showsData := s.radioPayloadFor(ctx, "get_radio_show_analytics", []string{"p_days", "p_start", "p_end"}, radioArgs)
	snapData := s.radioPayloadFor(ctx, "get_radio_show_snapshots", []string{"p_start", "p_end"}, map[string]any{"p_start": pStart, "p_end": pEnd})

	// Live nowplaying from AzuraCast for the "Now Playing" card.
	if radioData != nil {
		if cur, ok := s.fetchLiveNowPlaying(ctx); ok {
			radioData["current"] = cur
		}
	}

	s.mergeRadioAnalytics(merged, radioData, showsData, snapData)

	// Enrich on-demand rows with episode thumbnails (VOD content has no EPG
	// metadata, so the analytics RPC can't resolve a thumbnail for it).
	s.enrichOndemandThumbnails(ctx, merged)

	merged["radioSource"] = "azuraCast"
	merged["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	merged["is_crash_resilient"] = true

	writeJSON(w, http.StatusOK, merged)
}

// enrichOndemandThumbnails fills thumbnailUrl for topContent rows whose
// contentType is 'ondemand', using the on-demand episode catalog (cached).
func (s *server) enrichOndemandThumbnails(ctx context.Context, merged map[string]any) {
	tc, ok := merged["topContent"].([]any)
	if !ok {
		return
	}
	thumbs := s.episodeThumbnails(ctx)
	for _, item := range tc {
		m := mapAny(item)
		if strings.ToLower(str(m["contentType"])) != "ondemand" {
			continue
		}
		if str(m["thumbnailUrl"]) != "" {
			continue
		}
		if t := thumbs[str(m["contentId"])]; t != "" {
			m["thumbnailUrl"] = t
		}
	}
}

// episodeThumbnails returns the on-demand episode id -> thumbnail map, cached
// for an hour and rebuilt from PostgREST when stale (so per-request lookups are
// a plain map read).
func (s *server) episodeThumbnails(ctx context.Context) map[string]string {
	s.odThumbMu.Lock()
	defer s.odThumbMu.Unlock()
	if s.odThumbCache != nil && time.Now().Before(s.odThumbExpires) {
		return s.odThumbCache
	}
	raw, _, err := s.doRest(ctx, "episodes", url.Values{"select": {"id,thumbnail"}})
	if err != nil {
		if s.odThumbCache != nil {
			return s.odThumbCache // keep stale on failure
		}
		return nil
	}
	var rows []struct {
		ID        string `json:"id"`
		Thumbnail string `json:"thumbnail"`
	}
	_ = json.Unmarshal(raw, &rows)
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.ID != "" && r.Thumbnail != "" {
			m[r.ID] = r.Thumbnail
		}
	}
	s.odThumbCache = m
	s.odThumbExpires = time.Now().Add(time.Hour)
	return m
}

// radioPayloadFor runs a radio RPC and parses it, returning nil on any failure.
func (s *server) radioPayloadFor(ctx context.Context, rpc string, order []string, args map[string]any) map[string]any {
	payload, err := s.radioRPCPayload(ctx, rpc, order, args, rpcCacheTTL)
	if err != nil {
		log.Printf("admin analytics: %s failed: %v", rpc, err)
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(payload, &out); err != nil {
		log.Printf("admin analytics: %s bad payload: %v", rpc, err)
		return nil
	}
	return out
}

// mergeRadioAnalytics reproduces src/app/api/analytics/route.ts exactly:
// radio listening-time overrides, now-playing card, station performance FM
// override, top-content radio rows (snapshots → shows → songs), content-type
// breakdown, peak hours, watch-time distribution, and country/browser/stream
// breakdowns.
func (s *server) mergeRadioAnalytics(merged map[string]any, radioData, showsData, snapData map[string]any) {
	if radioData == nil {
		return
	}

	hourly := asSlice(radioData["hourly"])
	curAvg := num(merged["averageListeningTimeSeconds"])
	if len(hourly) > 0 {
		sum := 0.0
		for _, h := range hourly {
			sum += num(mapAny(h)["avg_listeners"])
		}
		merged["totalListeningTimeSeconds"] = int64(sum * 3600 / 24)
		merged["averageListeningTimeSeconds"] = int64(sum / float64(len(hourly)) * 60)
		curAvg = num(merged["averageListeningTimeSeconds"])
	}
	if curAvg == 0 {
		if byStream := asSlice(radioData["byStream"]); len(byStream) > 0 {
			totalConnected := 0.0
			totalListeners := 0.0
			for _, st := range byStream {
				m := mapAny(st)
				totalConnected += num(m["connected_seconds"])
				totalListeners += num(m["listeners"])
			}
			merged["totalListeningTimeSeconds"] = int64(totalConnected)
			if totalListeners > 0 {
				merged["averageListeningTimeSeconds"] = int64(totalConnected / totalListeners)
			} else {
				merged["averageListeningTimeSeconds"] = 0
			}
		}
	}

	if cur, ok := radioData["current"].(map[string]any); ok {
		merged["currentRadioListeners"] = int64(num(cur["listeners_total"]))
		merged["currentRadioSong"] = firstNonEmpty(str(cur["song_title"]), str(cur["song_text"]))
		merged["currentRadioArtist"] = str(cur["song_artist"])
	}

	if daily := asSlice(radioData["daily"]); len(daily) > 0 {
		if sp, ok := merged["stationPerformance"].([]any); ok {
			total := 0.0
			for _, d := range daily {
				total += num(mapAny(d)["listeners"])
			}
			for _, st := range sp {
				m := mapAny(st)
				if str(m["name"]) == "Salt FM" {
					if total > 0 {
						m["views"] = total
					}
					break
				}
			}
		}
	}

	if truthy(radioData["topSongs"]) || truthy(showsData["shows"]) || truthy(snapData["shows"]) {
		radioItems := s.buildRadioItems(radioData, showsData, snapData)

		if tc, ok := merged["topContent"].([]any); ok {
			nonRadio := make([]any, 0, len(tc))
			for _, c := range tc {
				if strings.ToLower(str(mapAny(c)["contentType"])) != "radio" {
					nonRadio = append(nonRadio, c)
				}
			}
			all := make([]any, 0, len(nonRadio)+len(radioItems))
			all = append(all, nonRadio...)
			for _, ri := range radioItems {
				all = append(all, ri)
			}
			sort.SliceStable(all, func(i, j int) bool {
				return num(mapAny(all[i])["views"]) > num(mapAny(all[j])["views"])
			})
			if len(all) > 40 {
				all = all[:40]
			}
			merged["topContent"] = all
		}

		radioViews := 0.0
		radioTime := 0.0
		for _, ri := range radioItems {
			m := mapAny(ri)
			radioViews += num(m["views"])
			radioTime += num(m["totalWatchTime"])
		}
		ctb, _ := merged["contentTypeBreakdown"].([]any)
		found := false
		for _, c := range ctb {
			m := mapAny(c)
			if str(m["type"]) == "radio" {
				if radioViews != 0 {
					m["views"] = radioViews
				}
				if radioTime != 0 {
					m["totalWatchTime"] = radioTime
				}
				found = true
				break
			}
		}
		if !found {
			uniqueUsers := 0.0
			if cur, ok := radioData["current"].(map[string]any); ok {
				uniqueUsers = num(cur["listeners_unique"])
			}
			ctb = append(ctb, map[string]any{
				"type":           "radio",
				"views":          radioViews,
				"uniqueUsers":    uniqueUsers,
				"totalWatchTime": radioTime,
			})
		}
		merged["contentTypeBreakdown"] = ctb
	}

	if len(hourly) > 0 {
		peak := make([]any, 0, len(hourly))
		for _, h := range hourly {
			m := mapAny(h)
			peak = append(peak, map[string]any{"hour": m["hour"], "views": num(m["avg_listeners"])})
		}
		merged["radioPeakHours"] = peak
	}

	if blt := asSlice(radioData["byListeningTime"]); len(blt) > 0 {
		dist, _ := merged["watchTimeDistribution"].([]any)
		filtered := make([]any, 0, len(dist))
		for _, d := range dist {
			if str(mapAny(d)["category"]) == "listen" {
				continue
			}
			filtered = append(filtered, d)
		}
		for _, t := range blt {
			m := mapAny(t)
			filtered = append(filtered, map[string]any{
				"category": "listen",
				"range":    m["label"],
				"count":    num(m["value"]),
			})
		}
		merged["watchTimeDistribution"] = filtered
	}

	for src, out := range map[string]string{"byCountry": "radioByCountry", "byBrowser": "radioByBrowser", "byStream": "radioByStream"} {
		if truthy(radioData[src]) {
			merged[out] = radioData[src]
		}
	}
	if truthy(radioData["best"]) {
		best := asSlice(radioData["best"])
		if len(best) > 5 {
			best = best[:5]
		}
		merged["radioBestMoments"] = best
	}
	if truthy(radioData["worst"]) {
		worst := asSlice(radioData["worst"])
		if len(worst) > 5 {
			worst = worst[:5]
		}
		merged["radioWorstMoments"] = worst
	}
}

// buildRadioItems builds the top-content radio rows: listener snapshots first,
// then history-based show analytics, then (filtered) top songs.
func (s *server) buildRadioItems(radioData, showsData, snapData map[string]any) []any {
	var items []any

	if shows := asSlice(snapData["shows"]); len(shows) > 0 {
		n := minInt(len(shows), 15)
		for _, sh := range shows[:n] {
			m := mapAny(sh)
			name := str(m["programName"])
			items = append(items, map[string]any{
				"contentId":      "snap_show_" + name,
				"contentName":    firstNonEmpty(name, "Untitled"),
				"contentType":    "radio",
				"station":        "Salt FM",
				"thumbnailUrl":   firstNonEmpty(str(m["thumbnail"]), str(m["image"])),
				"views":          orNum(m["peakListeners"], m["uniqueListeners"]),
				"totalWatchTime": int64(num(m["avgConnectedSeconds"]) * num(m["uniqueListeners"])),
				"avgListeners":   num(m["uniqueListeners"]),
				"peakListeners":  num(m["peakListeners"]),
				"startTime":      "",
				"endTime":        "",
				"days":           "",
			})
		}
	}

	if len(asSlice(snapData["shows"])) == 0 {
		if shows := asSlice(showsData["shows"]); len(shows) > 0 {
			n := minInt(len(shows), 15)
			for _, sh := range shows[:n] {
				m := mapAny(sh)
				name := str(m["programName"])
				items = append(items, map[string]any{
					"contentId":      "azura_show_" + name,
					"contentName":    firstNonEmpty(name, "Untitled"),
					"contentType":    "radio",
					"station":        "Salt FM",
					"thumbnailUrl":   firstNonEmpty(str(m["thumbnail"]), str(m["image"])),
					"views":          num(m["totalSongs"]),
					"totalWatchTime": int64(num(m["totalAirtimeSeconds"])),
					"avgListeners":   num(m["avgListeners"]),
					"peakListeners":  num(m["peakListeners"]),
					"startTime":      str(m["startTime"]),
					"endTime":        str(m["endTime"]),
					"days":           str(m["days"]),
				})
			}
		}
	}

	if topSongs := asSlice(radioData["topSongs"]); len(topSongs) > 0 {
		ignored := []string{"live broadcast", "salt fm luganda sfx chimes"}
		count := 0
		for _, sng := range topSongs {
			if count >= 5 {
				break
			}
			m := mapAny(sng)
			title := strings.ToLower(strings.TrimSpace(str(m["song_title"])))
			if title == "" {
				continue
			}
			skip := false
			for _, ign := range ignored {
				if strings.Contains(title, ign) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			items = append(items, map[string]any{
				"contentId":      "azura_song_" + str(m["song_title"]),
				"contentName":    firstNonEmpty(str(m["song_title"]), "Untitled"),
				"contentType":    "radio",
				"station":        "Salt FM",
				"thumbnailUrl":   "",
				"views":          num(m["plays"]),
				"totalWatchTime": int64(num(m["total_airtime_seconds"])),
				"avgListeners":   nil,
			})
			count++
		}
	}

	return items
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func asSlice(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func mapAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// orNum returns the first non-zero numeric value (mirrors JS `a || b || 0`).
func orNum(vals ...any) float64 {
	for _, v := range vals {
		if n := num(v); n != 0 {
			return n
		}
	}
	return 0
}

// truthy mirrors JS truthiness for the values produced by JSON unmarshalling.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return true
	}
	return true
}
