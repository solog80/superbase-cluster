package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// epgCacheEntry holds a built "today" EPG payload for a given date (24h TTL).
type epgCacheEntry struct {
	payload   []byte
	expiresAt time.Time
}

// epgStations / epgPrograms mirror the PostgREST rows from the Supabase primary.
type epgStation struct {
	ID            string  `json:"id"`
	LineupType    string  `json:"lineup_type"`
	StationURL    *string `json:"station_url"`
	IsLive        bool    `json:"is_live"`
	IsVisible     bool    `json:"is_visible"`
	IsPayPerView  bool    `json:"is_pay_per_view"`
	Price         *float64 `json:"price"`
	Currency      *string `json:"currency"`
	EnableChat    bool    `json:"enable_chat"`
}

type epgProgram struct {
	StationID   string  `json:"station_id"`
	ProgramName string  `json:"program_name"`
	Presenter   *string `json:"presenter"`
	Genre       *string `json:"genre"`
	Details     *string `json:"details"`
	Language    *string `json:"language"`
	StartTime   string  `json:"start_time"`
	EndTime     string  `json:"end_time"`
	Days        *string `json:"days"`
	Type        *string `json:"type"`
	Image       *string `json:"image"`
	Thumbnail   *string `json:"thumbnail"`
	TargetAud   *string `json:"target_audience"`
	TvProgramID *string `json:"tv_program_id"`
}

func (s *server) fetchEPG(ctx context.Context) ([]epgStation, []epgProgram, error) {
	raw, _, err := s.doRest(ctx, "epg_stations", url.Values{"select": {"*"}})
	if err != nil {
		return nil, nil, err
	}
	var stations []epgStation
	if err := json.Unmarshal(raw, &stations); err != nil {
		return nil, nil, err
	}
	raw2, _, err := s.doRest(ctx, "epg_programs", url.Values{"select": {"*"}})
	if err != nil {
		return nil, nil, err
	}
	var programs []epgProgram
	if err := json.Unmarshal(raw2, &programs); err != nil {
		return nil, nil, err
	}
	return stations, programs, nil
}

// programOnDay reports whether the program's comma-separated `days` includes
// the given weekday name (e.g. "Monday"). Mirrors isProgramValidForToday's
// same-day check.
func programOnDay(program epgProgram, dayName string) bool {
	if program.Days == nil {
		return false
	}
	for _, d := range strings.Split(*program.Days, ",") {
		if strings.TrimSpace(d) == dayName {
			return true
		}
	}
	return false
}

// isCrossoverFromYesterday mirrors isProgramValidForToday's midnight-crossover
// check: program runs on yesterday's day AND its end time is earlier than its
// start (i.e. it crosses midnight into today).
func isCrossoverFromYesterday(program epgProgram, yesterdayDay string) bool {
	if !programOnDay(program, yesterdayDay) {
		return false
	}
	sh, eh := parseHour(program.StartTime), parseHour(program.EndTime)
	return eh < sh
}

func parseHour(hm string) int {
	if i := strings.IndexByte(hm, ':'); i > 0 {
		var h int
		fmt.Sscanf(hm[:i], "%d", &h)
		return h
	}
	return 0
}

// buildEPGPayload assembles the exact getEPGData response shape:
//
//	{ data: { tv: { stationKey: {stationImageUrl,... programs:[...] } },
//	          radio: { streamUrl, ... programs:[...] } }, source, cached }
//
// TV stations are keyed by station id; radio is a single object. Only
// stations that are visible and have today's programs are included.
func (s *server) buildEPGPayload(stations []epgStation, programs []epgProgram, dayName string, now time.Time) map[string]any {
	yesterday := now.AddDate(0, 0, -1)
	yesterdayDay := yesterday.UTC().Format("Monday")

	radioStation := map[string]any{}
	tvData := map[string]any{}

	for _, st := range stations {
		if !st.IsVisible {
			continue
		}
		stationPrograms := filterPrograms(programs, st.ID, dayName, yesterdayDay)

		// Radio: single object keyed by nothing — build separately.
		if st.LineupType == "radio" {
			radioStation["stationUrl"] = orEmptyStr(st.StationURL)
			radioStation["stationImageUrl"] = nil
			radioStation["isPayPerView"] = st.IsPayPerView
			radioStation["price"] = priceString(st.Price)
			radioStation["currency"] = orEmptyStr(st.Currency)
			radioStation["isLive"] = st.IsLive
			radioStation["programs"] = stationPrograms
			continue
		}

		// TV: include only if it has today's programs.
		if len(stationPrograms) == 0 {
			continue
		}
		tvData[st.ID] = map[string]any{
			"stationImageUrl": nil,
			"stationUrl":      orEmptyStr(st.StationURL),
			"isPayPerView":    st.IsPayPerView,
			"price":           priceString(st.Price),
			"currency":        orEmptyStr(st.Currency),
			"isLive":          st.IsLive,
			"programs":        stationPrograms,
		}
	}

	return map[string]any{
		"data": map[string]any{
			"tv":    tvData,
			"radio": radioStation,
		},
	}
}

// filterPrograms returns programs for a station that air today (same day or
// crossing midnight from yesterday), shaped exactly like the app expects.
func filterPrograms(programs []epgProgram, stationID, dayName, yesterdayDay string) []map[string]any {
	var out []map[string]any
	for _, p := range programs {
		if p.StationID != stationID {
			continue
		}
		if !programOnDay(p, dayName) && !isCrossoverFromYesterday(p, yesterdayDay) {
			continue
		}
		out = append(out, map[string]any{
			"programName": p.ProgramName,
			"presenter":   orNilStr(p.Presenter),
			"genre":       orNilStr(p.Genre),
			"details":     orNilStr(p.Details),
			"language":    orNilStr(p.Language),
			"startTime":   p.StartTime,
			"endTime":     p.EndTime,
			"days":        orEmptyStr(p.Days),
			"type":        orNilStr(p.Type),
			"image":       orNilStr(p.Image),
			"thumbnail":   orNilStr(p.Thumbnail),
		})
	}
	return out
}

func orEmptyStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// priceString formats a numeric price as a string (the app's Station model
// expects String?); nil → "".
func priceString(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%g", *p)
}

func orNilStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// handleGetEPGData mirrors epg.js getEPGData: returns today's TV + radio
// lineup, cached in-memory keyed by date (24h TTL).
func (s *server) handleGetEPGData(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	dateKey := now.Format("2006-01-02")

	s.epgMu.Lock()
	if e, ok := s.epgCache[dateKey]; ok && time.Now().Before(e.expiresAt) {
		s.epgMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(e.payload)
		return
	}
	s.epgMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	stations, programs, err := s.fetchEPG(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}

	dayName := now.Format("Monday")
	payload := s.buildEPGPayload(stations, programs, dayName, now)
	payload["source"] = "db"
	payload["cached"] = false
	payload["timestamp"] = now.Format(time.RFC3339)

	raw, _ := json.Marshal(payload)
	s.epgMu.Lock()
	s.epgCache[dateKey] = epgCacheEntry{payload: raw, expiresAt: time.Now().Add(24 * time.Hour)}
	s.epgMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

// handleGetAdminEPGData mirrors epg.js getAdminEPGData: returns ALL programs
// (no date filtering) for the admin dashboard.
func (s *server) handleGetAdminEPGData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	stations, programs, err := s.fetchEPG(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}

	tvData := map[string]any{}
	radioData := map[string]any{}
	for _, st := range stations {
		if !st.IsVisible {
			continue
		}
		stationPrograms := make([]map[string]any, 0)
		for _, p := range programs {
			if p.StationID != st.ID {
				continue
			}
			stationPrograms = append(stationPrograms, map[string]any{
				"programName":    p.ProgramName,
				"tvProgramId":    orEmptyStr(p.TvProgramID),
				"presenter":      orEmptyStr(p.Presenter),
				"genre":          orEmptyStr(p.Genre),
				"language":       orEmptyStr(p.Language),
				"details":        orEmptyStr(p.Details),
				"startTime":      p.StartTime,
				"endTime":        p.EndTime,
				"targetAudience": orEmptyStr(p.TargetAud),
				"days":           orEmptyStr(p.Days),
				"type":           orEmptyStr(p.Type),
				"image":          orEmptyStr(p.Image),
				"thumbnail":      orEmptyStr(p.Thumbnail),
			})
		}
		if st.LineupType == "radio" {
			radioData["stationUrl"] = orEmptyStr(st.StationURL)
			radioData["programs"] = stationPrograms
		} else {
			tvData[st.ID] = map[string]any{
				"stationImageUrl": nil,
				"stationUrl":      orEmptyStr(st.StationURL),
				"isPayPerView":    st.IsPayPerView,
				"price":           priceString(st.Price),
				"currency":        orEmptyStr(st.Currency),
				"isLive":          st.IsLive,
				"programs":        stationPrograms,
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":      map[string]any{"tv": tvData, "radio": radioData},
		"source":    "db",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// clearEPGCache clears the 3-day EPG cache window (yesterday/today/tomorrow).
func (s *server) clearEPGCache() []string {
	now := time.Now().UTC()
	keys := []string{
		now.Format("2006-01-02"),
		now.AddDate(0, 0, 1).Format("2006-01-02"),
		now.AddDate(0, 0, -1).Format("2006-01-02"),
	}
	s.epgMu.Lock()
	for _, k := range keys {
		delete(s.epgCache, k)
	}
	s.epgMu.Unlock()
	return keys
}

// handleInvalidateEPGCache clears today's (and tomorrow's) EPG cache keys so
// the next request rebuilds from Postgres. Mirrors invalidateEPGCache.
func (s *server) handleInvalidateEPGCache(w http.ResponseWriter, r *http.Request) {
	keys := s.clearEPGCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"message":    fmt.Sprintf("Invalidated %d EPG cache keys", len(keys)),
		"keysCleared": keys,
	})
}

// handleEpgHealthCheck mirrors epgHealthCheck: pings Postgres via a cheap
// query and reports status.
func (s *server) handleEpgHealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	_, _, err := s.doRest(ctx, "epg_stations", url.Values{"select": {"id"}, "limit": {"1"}})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "degraded",
			"postgres":  "offline",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "healthy",
		"postgres":  "online",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleTriggerEPGSync mirrors epg.js triggerEPGSync / syncEPGToBigQuery.
func (s *server) handleTriggerEPGSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	count, err := s.syncEPGMetadata(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "EPG Sync triggered", "count": count})
}

// syncEPGMetadata mirrors the EPG lineup into the TSDB epg_metadata table so
// the analytics readers can resolve station names + thumbnails. Idempotent
// (replaces all). Returns the row count.
func (s *server) syncEPGMetadata(ctx context.Context) (int, error) {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return 0, err
	}
	stations, programs, err := s.fetchEPG(ctx)
	if err != nil {
		return 0, err
	}

	type metaRow struct {
		contentID    string
		contentName  string
		contentType  string
		stationName  string
		thumbnailURL string
	}
	rows := []metaRow{}

	// TV stations + programs.
	for _, st := range stations {
		if st.LineupType != "tv" {
			continue
		}
		stationLabel := st.ID
		if strings.Contains(strings.ToLower(st.ID), "one") {
			stationLabel = "Salt TV One"
		} else if strings.Contains(strings.ToLower(st.ID), "two") {
			stationLabel = "Salt TV Two"
		}
		thumb := ""
		if st.StationURL != nil {
			thumb = *st.StationURL
		}
		rows = append(rows, metaRow{st.ID, st.ID, "tv", stationLabel, thumb})
		for _, p := range programs {
			if p.StationID != st.ID {
				continue
			}
			img := ""
			if p.Image != nil {
				img = *p.Image
			}
			pid := ""
			if p.TvProgramID != nil {
				pid = *p.TvProgramID
			}
			rows = append(rows, metaRow{pid, p.ProgramName, "tv", stationLabel, img})
		}
	}

	// Radio.
	rows = append(rows, metaRow{"live_stream", "Salt FM Live", "radio", "Salt FM", ""})
	for _, st := range stations {
		if st.LineupType != "radio" {
			continue
		}
		for _, p := range programs {
			if p.StationID != st.ID {
				continue
			}
			img := ""
			if p.Thumbnail != nil {
				img = *p.Thumbnail
			} else if p.Image != nil {
				img = *p.Image
			}
			rows = append(rows, metaRow{p.ProgramName, p.ProgramName, "radio", "Salt FM", img})
		}
	}

	if len(rows) == 0 {
		return 0, nil
	}

	// Replace epg_metadata (truncate then insert) on TSDB.
	if _, err := db.ExecContext(ctx, "TRUNCATE public.epg_metadata"); err != nil {
		return 0, err
	}
	meta := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		meta = append(meta, map[string]any{
			"content_id":    r.contentID,
			"content_name":  r.contentName,
			"content_type":  r.contentType,
			"station_name":  r.stationName,
			"thumbnail_url": r.thumbnailURL,
			"last_updated":  time.Now().UTC().Format(time.RFC3339),
		})
	}
	if err := s.insertRows(ctx, "epg_metadata", meta); err != nil {
		return 0, err
	}

	// Also refresh the radio programs table used by the radio shows/reports
	// RPCs (was holding stale pre-migration Firebase/Google image URLs).
	radioN, radioErr := s.syncRadioPrograms(ctx)
	if radioErr != nil {
		return len(rows), radioErr
	}
	_ = radioN

	return len(rows), nil
}
