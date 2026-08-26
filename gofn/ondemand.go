package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ondemandCacheEntry holds the fully-built on-demand payload (both full and
// published-filtered), keyed in memory with a 6-hour TTL, mirroring the
// Firestore `ondemand:all` / `ondemand:published` Redis keys.
type ondemandCacheEntry struct {
	full      []byte // all shows (dashboard, includes unpublished)
	published []byte // published-only (app)
	expiresAt time.Time
	builtAt   time.Time
}

// odShow / odSeason / odEpisode mirror the PostgREST rows.
type odShow struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Type         *string `json:"type"`
	Description  *string `json:"description"`
	Thumbnail    *string `json:"thumbnail"`
	PosterURL16x9 *string `json:"poster_url_16x9"`
	PosterURL2x3 *string `json:"poster_url_2x3"`
	SeasonCount  int     `json:"season_count"`
	Published    bool    `json:"published"`
	CreatedAt    *string `json:"created_at"`
	BunnyGUID    *string `json:"bunny_guid"`
}

type odSeason struct {
	ID           string `json:"id"`
	ShowID       string `json:"show_id"`
	Title        string `json:"title"`
	Ord          int    `json:"ord"`
	EpisodeCount int    `json:"episode_count"`
	Published    bool   `json:"published"`
	CreatedAt    *string `json:"created_at"`
}

type odEpisode struct {
	ID          string  `json:"id"`
	SeasonID    string  `json:"season_id"`
	ShowID      string  `json:"show_id"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Duration    int     `json:"duration"`
	Thumbnail   *string `json:"thumbnail"`
	VideoURL    *string `json:"video_url"`
	DateUploaded *string `json:"date_uploaded"`
	AirDate     *string `json:"air_date"`
	Published   bool    `json:"published"`
	Processing  bool    `json:"processing"`
	SfxJobName  *string `json:"sfx_job_name"`
	Server      *string `json:"server"`
	BunnyGUID   *string `json:"bunny_guid"`
}

func (s *server) fetchOnDemand(ctx context.Context) ([]odShow, []odSeason, []odEpisode, error) {
	raw, _, err := s.doRest(ctx, "tv_shows", url.Values{
		"select": {"*"}, "order": {"created_at.asc"},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	var shows []odShow
	if err := json.Unmarshal(raw, &shows); err != nil {
		return nil, nil, nil, err
	}

	raw, _, err = s.doRest(ctx, "seasons", url.Values{
		"select": {"*"}, "order": {"show_id.asc,ord.asc"},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	var seasons []odSeason
	if err := json.Unmarshal(raw, &seasons); err != nil {
		return nil, nil, nil, err
	}

	raw, _, err = s.doRest(ctx, "episodes", url.Values{
		"select": {"*"}, "order": {"show_id.asc,season_id.asc,id.asc"},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	var episodes []odEpisode
	if err := json.Unmarshal(raw, &episodes); err != nil {
		return nil, nil, nil, err
	}

	return shows, seasons, episodes, nil
}

// episodeJSON shapes an episode exactly as the app/admin expect (camelCase,
// Firestore-compatible keys). Fields mirror the Firestore episode doc.
func episodeJSON(e odEpisode) map[string]any {
	return map[string]any{
		"id":          e.ID,
		"title":       e.Title,
		"description": orEmpty(e.Description),
		"duration":    e.Duration,
		"thumbnail":   orEmpty(e.Thumbnail),
		"videoUrl":    orEmpty(e.VideoURL),
		"dateUploaded": orNilTS(e.DateUploaded),
		"airDate":     orNilTS(e.AirDate),
		"published":   e.Published,
		"processing":  e.Processing,
		"sfxJobName":  orEmpty(e.SfxJobName),
	}
}

// seasonJSON shapes a season (camelCase, Firestore-compatible). When
// publishedOnly is true, unpublished episodes are excluded.
func seasonJSON(se odSeason, episodes []odEpisode, publishedOnly bool) map[string]any {
	epList := make([]map[string]any, 0, len(episodes))
	for _, e := range episodes {
		if publishedOnly && !e.Published {
			continue
		}
		epList = append(epList, episodeJSON(e))
	}
	return map[string]any{
		"id":           se.ID,
		"title":        se.Title,
		"order":        se.Ord,
		"episodeCount": len(epList),
		"episodes":     epList,
		"published":    se.Published,
	}
}

// showJSON shapes a show with its seasons/episodes, matching buildCompleteOnDemandData.
func showJSON(sh odShow, seasons []odSeason, episodes []odEpisode) map[string]any {
	seasonList := make([]map[string]any, 0, len(seasons))
	for _, se := range seasons {
		eps := make([]odEpisode, 0)
		for _, e := range episodes {
			if e.SeasonID == se.ID {
				eps = append(eps, e)
			}
		}
		seasonList = append(seasonList, seasonJSON(se, eps, false))
	}
	return map[string]any{
		"id":          sh.ID,
		"title":       sh.Title,
		"type":        odNil(sh.Type),
		"description": orEmpty(sh.Description),
		"thumbnail":   orEmpty(sh.Thumbnail),
		"posterUrl16x9": orEmpty(sh.PosterURL16x9),
		"posterUrl2x3":  orEmpty(sh.PosterURL2x3),
		"seasonCount": sh.SeasonCount,
		"createdAt":   odNil(sh.CreatedAt),
		"bunnyGuid":   orEmpty(sh.BunnyGUID),
		"seasons":     seasonList,
		"published":   sh.Published,
	}
}

// buildOnDemandPayload assembles { shows: [...] } from the three tables,
// reversing seasons within each show (newest first, matching Firestore
// `seasons.reverse()`).
func buildOnDemandPayload(shows []odShow, seasons []odSeason, episodes []odEpisode, publishedOnly bool) map[string]any {
	byShow := map[string][]odSeason{}
	for _, se := range seasons {
		byShow[se.ShowID] = append(byShow[se.ShowID], se)
	}
	bySeason := map[string][]odEpisode{}
	for _, e := range episodes {
		bySeason[e.SeasonID] = append(bySeason[e.SeasonID], e)
	}

	out := make([]map[string]any, 0, len(shows))
	for _, sh := range shows {
		if publishedOnly && !sh.Published {
			continue
		}
		showSeasons := byShow[sh.ID]
		// Reverse so newest first (Firestore's seasons.reverse()).
		rev := make([]odSeason, 0, len(showSeasons))
		for i := len(showSeasons) - 1; i >= 0; i-- {
			rev = append(rev, showSeasons[i])
		}

		seasonList := make([]map[string]any, 0, len(rev))
		for _, se := range rev {
			if publishedOnly && !se.Published {
				continue
			}
			seasonList = append(seasonList, seasonJSON(se, bySeason[se.ID], publishedOnly))
		}

		// In published-only mode a show with zero published episodes is hidden
		// entirely (matches get_on_demand_catalog). Keep it only when it still
		// has at least one episode.
		if publishedOnly {
			pubEpisodes := 0
			for _, se := range rev {
				if !se.Published {
					continue
				}
				for _, e := range bySeason[se.ID] {
					if e.Published {
						pubEpisodes++
					}
				}
			}
			if pubEpisodes == 0 {
				continue
			}
		}

		out = append(out, map[string]any{
			"id":          sh.ID,
			"title":       sh.Title,
			"type":        odNil(sh.Type),
			"description": orEmpty(sh.Description),
			"thumbnail":   orEmpty(sh.Thumbnail),
			"posterUrl16x9": orEmpty(sh.PosterURL16x9),
			"posterUrl2x3":  orEmpty(sh.PosterURL2x3),
			"seasonCount": sh.SeasonCount,
			"createdAt":   odNil(sh.CreatedAt),
			"bunnyGuid":   orEmpty(sh.BunnyGUID),
			"seasons":     seasonList,
			"published":   sh.Published,
		})
	}
	return map[string]any{"shows": out}
}

// ondemandPayload returns the full + published byte payloads, caching in
// memory for 6h (mirrors getOnDemandData's Redis caching). The third return
// reports whether the payload came from cache (to set source/cached flags).
// The cache is also invalidated when the DB is newer than the cache build
// (catches direct-DB edits that bypass the Go CRUD handlers).
func (s *server) ondemandPayload(ctx context.Context) (full, published []byte, fromCache bool, err error) {
	s.odMu.Lock()
	cache := s.odCache
	s.odMu.Unlock()
	if cache != nil && time.Now().Before(cache.expiresAt) {
		fresh, ferr := s.onDemandStale(ctx, cache.builtAt)
		if ferr != nil {
			// If the staleness probe fails, trust the TTL and serve cache.
			return cache.full, cache.published, true, nil
		}
		if !fresh {
			return cache.full, cache.published, true, nil
		}
		// DB changed since we built the cache — rebuild below.
	}

	shows, seasons, episodes, err := s.fetchOnDemand(ctx)
	if err != nil {
		return nil, nil, false, err
	}

	now := time.Now().UTC()
	builtAt := now
	fullData := buildOnDemandPayload(shows, seasons, episodes, false)
	fullPayload := map[string]any{
		"data":      fullData,
		"source":    "db",
		"cached":    false,
		"timestamp": now.Format(time.RFC3339),
	}
	fullJSON, _ := json.Marshal(fullPayload)

	pubData := buildOnDemandPayload(shows, seasons, episodes, true)
	pubPayload := map[string]any{
		"data":      pubData,
		"source":    "db",
		"cached":    false,
		"timestamp": now.Format(time.RFC3339),
	}
	pubJSON, _ := json.Marshal(pubPayload)

	s.odMu.Lock()
	s.odCache = &ondemandCacheEntry{
		full:      fullJSON,
		published: pubJSON,
		expiresAt: time.Now().Add(6 * time.Hour),
		builtAt:   builtAt,
	}
	s.odMu.Unlock()

	return fullJSON, pubJSON, false, nil
}

// onDemandStale reports whether any on-demand table changed after the cache
// was built. Uses a cheap max(updated_at) probe per table (order desc, limit 1)
// so direct-DB edits that bypass the Go CRUD still invalidate the cache.
func (s *server) onDemandStale(ctx context.Context, builtAt time.Time) (bool, error) {
	for _, tbl := range []string{"tv_shows", "seasons", "episodes"} {
		raw, _, err := s.doRest(ctx, tbl, url.Values{
			"select": {"updated_at"}, "order": {"updated_at.desc"}, "limit": {"1"},
		})
		if err != nil {
			return false, err
		}
		var rows []struct {
			UpdatedAt *string `json:"updated_at"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return false, err
		}
		if len(rows) == 0 || rows[0].UpdatedAt == nil {
			continue
		}
		ts, perr := time.Parse(time.RFC3339Nano, *rows[0].UpdatedAt)
		if perr != nil {
			if ts, perr = time.Parse(time.RFC3339, *rows[0].UpdatedAt); perr != nil {
				continue
			}
		}
		if ts.After(builtAt.Add(-2 * time.Second)) {
			return true, nil
		}
	}
	return false, nil
}

// handleGetOnDemandData mirrors getOnDemandData: returns ALL shows (including
// unpublished) for the admin dashboard.
func (s *server) handleGetOnDemandData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	full, _, fromCache, err := s.ondemandPayload(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if fromCache {
		full = s.markCached(full)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(full)
}

// handleGetPublicOnDemandData mirrors getPublicOnDemandData: returns only
// published shows/seasons/episodes for the app.
func (s *server) handleGetPublicOnDemandData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, pub, fromCache, err := s.ondemandPayload(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if fromCache {
		pub = s.markCached(pub)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(pub)
}

// markCached rewrites the payload's source/cached flags so cache hits report
// themselves honestly (mirrors the Redis-cache-hit response shape).
func (s *server) markCached(raw []byte) []byte {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		m["source"] = "cache"
		m["cached"] = true
		if out, err := json.Marshal(m); err == nil {
			return out
		}
	}
	return raw
}

// handleGetOnDemandShowById mirrors getOnDemandShowById: returns one show
// with all seasons + episodes, reading from the cached payload.
func (s *server) handleGetOnDemandShowById(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID string `json:"showId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ShowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "showId is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	full, _, _, err := s.ondemandPayload(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var parsed struct {
		Data struct {
			Shows []map[string]any `json:"shows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(full, &parsed); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	for _, sh := range parsed.Data.Shows {
		if sh["id"] == body.ShowID {
			writeJSON(w, http.StatusOK, map[string]any{"data": sh, "source": "db", "cached": false})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Show " + body.ShowID + " not found"})
}

// handleGetOnDemandSeasonEpisodes mirrors getOnDemandSeasonEpisodes: returns
// one season's episodes from the cached payload.
func (s *server) handleGetOnDemandSeasonEpisodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID   string `json:"showId"`
		SeasonID string `json:"seasonId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ShowID == "" || body.SeasonID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "showId and seasonId are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	full, _, _, err := s.ondemandPayload(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var parsed struct {
		Data struct {
			Shows []map[string]any `json:"shows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(full, &parsed); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	for _, sh := range parsed.Data.Shows {
		if sh["id"] != body.ShowID {
			continue
		}
		seasons, _ := sh["seasons"].([]any)
		for _, sv := range seasons {
			smap, _ := sv.(map[string]any)
			if smap["id"] != body.SeasonID {
				continue
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"showId":      body.ShowID,
					"seasonId":    body.SeasonID,
					"seasonTitle": smap["title"],
					"seasonOrder": smap["order"],
					"episodes":    smap["episodes"],
				},
				"source": "db",
				"cached": false,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Season " + body.SeasonID + " not found in show " + body.ShowID})
}

// handleOndemandHealthCheck mirrors ondemandHealthCheck: pings Postgres.
func (s *server) handleOndemandHealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	_, _, err := s.doRest(ctx, "tv_shows", url.Values{"select": {"id"}, "limit": {"1"}})
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

// clearOndemandCache drops the in-memory payload so the next read rebuilds.
func (s *server) clearOndemandCache() {
	s.odMu.Lock()
	s.odCache = nil
	s.odMu.Unlock()
}

func orEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func odNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func orNilTS(p *string) any {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return v
}
