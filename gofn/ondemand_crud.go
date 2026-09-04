package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ──────────────── ON-DEMAND SHOW CRUD ────────────────

// handleUpdateOnDemandShow mirrors updateOnDemandShow: updates show metadata.
func (s *server) handleUpdateOnDemandShow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID  string         `json:"showId"`
		Updates map[string]any `json:"updates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.Updates == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId and updates are required"})
		return
	}

	colMap := map[string]string{
		"title": "title", "type": "type", "description": "description",
		"thumbnail": "thumbnail", "posterUrl16x9": "poster_url_16x9",
		"posterUrl2x3": "poster_url_2x3", "bunnyGuid": "bunny_guid",
		"published": "published", "seasonCount": "season_count",
	}
	row := map[string]any{}
	for k, v := range body.Updates {
		if col, ok := colMap[k]; ok {
			row[col] = v
		}
	}
	row["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if len(row) <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no valid update fields"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	affected, err := s.restPatchCount(ctx, "tv_shows", "id=eq."+url.QueryEscape(body.ShowID), row)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no show matched the given id"})
		return
	}
	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Show updated successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// handleCreateOnDemandShow mirrors createOnDemandShow: creates a new show.
func (s *server) handleCreateOnDemandShow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}

	suffix, _ := randomHex(3)
	showID := generateSlug(body.Title) + "_" + suffix
	now := time.Now().UTC().Format(time.RFC3339)

	row := map[string]any{
		"id":          showID,
		"title":       strings.TrimSpace(body.Title),
		"type":        orStr(body.Type, "series"),
		"description": body.Description,
		"season_count": 0,
		"published":   true,
		"created_at":  now,
		"updated_at":  now,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.restPostRow(ctx, "tv_shows", row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"show":    map[string]any{"id": showID, "title": strings.TrimSpace(body.Title)},
		"message": "Show created successfully",
	})
}

// handleDeleteOnDemandShow mirrors deleteOnDemandShow: deletes a show (cascade
// removes seasons + episodes via FK).
func (s *server) handleDeleteOnDemandShow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID string `json:"showId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.restDelete(ctx, "tv_shows", "id=eq."+url.QueryEscape(body.ShowID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Show deleted successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// ──────────────── ON-DEMAND SEASON CRUD ────────────────

// handleCreateOnDemandSeason mirrors createOnDemandSeason: creates a season.
func (s *server) handleCreateOnDemandSeason(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID string `json:"showId"`
		Title  string `json:"title"`
		Order  int    `json:"order"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId and title are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Resolve the show title for slugging.
	var showTitle string
	if raw, _, err := s.doRest(ctx, "tv_shows", url.Values{"select": {"title"}, "id": {"eq." + url.QueryEscape(body.ShowID)}}); err == nil {
		var rows []struct{ Title string `json:"title"` }
		if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
			showTitle = rows[0].Title
		}
	}

	seasonOrder := body.Order
	if seasonOrder == 0 {
		// next season order = max(ord) + 1
		if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"ord"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}, "order": {"ord.desc"}, "limit": {"1"}}); err == nil {
			var rows []struct {
				Ord int `json:"ord"`
			}
			if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
				seasonOrder = rows[0].Ord + 1
			} else {
				seasonOrder = 1
			}
		} else {
			seasonOrder = 1
		}
	}

	showSlug := generateSlug(showTitle)
	seasonNum := fmt.Sprintf("%02d", seasonOrder)
	seasonID := showSlug + "_s" + seasonNum
	now := time.Now().UTC().Format(time.RFC3339)

	row := map[string]any{
		"id": seasonID, "show_id": body.ShowID, "title": body.Title,
		"ord": seasonOrder, "episode_count": 0, "published": true,
		"created_at": now, "updated_at": now,
	}
	if err := s.restPostRow(ctx, "seasons", row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Bump the show's seasonCount.
	if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"id"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}}); err == nil {
		var rows []struct{ ID string `json:"id"` }
		if json.Unmarshal(raw, &rows) == nil {
			_ = s.restPatch(ctx, "tv_shows", "id=eq."+url.QueryEscape(body.ShowID), map[string]any{"season_count": len(rows), "updated_at": now})
		}
	}

	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"season":  map[string]any{"id": seasonID, "title": body.Title, "order": seasonOrder},
		"message": "Season created successfully",
	})
}

// handleUpdateOnDemandSeason mirrors updateOnDemandSeason.
func (s *server) handleUpdateOnDemandSeason(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID   string         `json:"showId"`
		SeasonID string         `json:"seasonId"`
		Updates  map[string]any `json:"updates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.SeasonID == "" || body.Updates == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId, seasonId, and updates are required"})
		return
	}

	colMap := map[string]string{
		"title": "title", "order": "ord", "published": "published",
		"episodeCount": "episode_count",
	}
	row := map[string]any{}
	for k, v := range body.Updates {
		if col, ok := colMap[k]; ok {
			row[col] = v
		}
	}
	row["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if len(row) <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no valid update fields"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	filter := "id=eq." + url.QueryEscape(body.SeasonID) + "&show_id=eq." + url.QueryEscape(body.ShowID)
	affected, err := s.restPatchCount(ctx, "seasons", filter, row)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no season matched the given show/season ids"})
		return
	}
	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Season updated successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// handleDeleteOnDemandSeason mirrors deleteOnDemandSeason: deletes a season
// and all its episodes (FK cascade).
func (s *server) handleDeleteOnDemandSeason(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID   string `json:"showId"`
		SeasonID string `json:"seasonId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.SeasonID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId and seasonId are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	filter := "id=eq." + url.QueryEscape(body.SeasonID) + "&show_id=eq." + url.QueryEscape(body.ShowID)
	if err := s.restDelete(ctx, "seasons", filter); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Recompute show seasonCount.
	if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"id"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}}); err == nil {
		var rows []struct{ ID string `json:"id"` }
		if json.Unmarshal(raw, &rows) == nil {
			_ = s.restPatch(ctx, "tv_shows", "id=eq."+url.QueryEscape(body.ShowID), map[string]any{"season_count": len(rows), "updated_at": time.Now().UTC().Format(time.RFC3339)})
		}
	}

	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Season deleted successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// ──────────────── ON-DEMAND EPISODE CRUD ────────────────

// handleUpdateOnDemandEpisode mirrors updateOnDemandEpisode.
func (s *server) handleUpdateOnDemandEpisode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID    string         `json:"showId"`
		SeasonID  string         `json:"seasonId"`
		EpisodeID string         `json:"episodeId"`
		Updates   map[string]any `json:"updates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.SeasonID == "" || body.EpisodeID == "" || body.Updates == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId, seasonId, episodeId, and updates are required"})
		return
	}

	colMap := map[string]string{
		"title": "title", "description": "description", "duration": "duration",
		"thumbnail": "thumbnail", "videoUrl": "video_url",
		"dateUploaded": "date_uploaded", "airDate": "air_date",
		"published": "published", "processing": "processing",
		"sfxJobName": "sfx_job_name",
	}
	row := map[string]any{}
	for k, v := range body.Updates {
		if col, ok := colMap[k]; ok {
			row[col] = v
		}
	}
	row["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if len(row) <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no valid update fields"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	filter := "id=eq." + url.QueryEscape(body.EpisodeID) + "&season_id=eq." + url.QueryEscape(body.SeasonID)
	affected, err := s.restPatchCount(ctx, "episodes", filter, row)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no episode matched the given show/season/episode ids",
		})
		return
	}
	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Episode updated successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// handleDeleteOnDemandEpisode mirrors deleteOnDemandEpisode.
func (s *server) handleDeleteOnDemandEpisode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID    string `json:"showId"`
		SeasonID  string `json:"seasonId"`
		EpisodeID string `json:"episodeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.SeasonID == "" || body.EpisodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId, seasonId, and episodeId are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	filter := "id=eq." + url.QueryEscape(body.EpisodeID) + "&season_id=eq." + url.QueryEscape(body.SeasonID)
	if err := s.restDelete(ctx, "episodes", filter); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Recompute season episodeCount.
	if raw, _, err := s.doRest(ctx, "episodes", url.Values{"select": {"id"}, "season_id": {"eq." + url.QueryEscape(body.SeasonID)}}); err == nil {
		var rows []struct{ ID string `json:"id"` }
		if json.Unmarshal(raw, &rows) == nil {
			_ = s.restPatch(ctx, "seasons", "id=eq."+url.QueryEscape(body.SeasonID), map[string]any{"episode_count": len(rows), "updated_at": time.Now().UTC().Format(time.RFC3339)})
		}
	}

	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Episode deleted successfully", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// ──────────────── CREATE SFX EPISODE ────────────────

// handleCreateSfxEpisode mirrors createSfxEpisode: creates an episode (and
// season if needed) for an SFX (objects.solofx.net) video after TUS upload.
// Also serves Bunny episodes: pass server="bunny" with a Bunny videoId (or
// bunnyGuid + videoUrl/thumbnail/duration) — the mesh is authoritative and the
// dashboard mirrors to Firestore afterwards (mesh-first dual write).
func (s *server) handleCreateSfxEpisode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID      string `json:"showId"`
		Title       string `json:"title"`
		Description string `json:"description"`
		VideoURL    string `json:"videoUrl"`
		SfxJobName  string `json:"sfxJobName"`
		Duration    int    `json:"duration"`
		Thumbnail   string `json:"thumbnail"`
		SeasonID    string `json:"seasonId"`
		SeasonTitle string `json:"seasonTitle"`
		Published   *bool  `json:"published"`
		Processing  *bool  `json:"processing"`
		// Bunny fields (server="bunny" path).
		Server    string `json:"server"`    // "sfx" (default) | "bunny"
		VideoID   string `json:"videoId"`   // Bunny video GUID to poll
		BunnyGUID string `json:"bunnyGuid"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId and title are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	// Bunny path: poll Bunny for metadata when a videoId is supplied and no
	// videoUrl/duration/thumbnail were passed through.
	server := strings.ToLower(body.Server)
	if server != "bunny" && server != "" && server != "sfx" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server must be 'sfx' or 'bunny'"})
		return
	}
	if server == "" {
		server = "sfx"
	}
	if server == "bunny" {
		bunnyGuid := body.BunnyGUID
		if bunnyGuid == "" {
			bunnyGuid = body.VideoID
		}
		if bunnyGuid == "" && body.VideoURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "videoId/bunnyGuid or videoUrl required for bunny episodes"})
			return
		}
		// If the caller only gave us a Bunny GUID (not final URLs), poll Bunny
		// for the ready video (mirrors the old Firebase createEpisodeFromBunnyUpload).
		if body.VideoURL == "" {
			v, err := fetchBunnyVideo(s.client, bunnyGuid)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "bunny: " + err.Error()})
				return
			}
			host := bunnyCDNHostname()
			if v.Hostname != "" {
				host = v.Hostname
			}
			body.VideoURL = fmt.Sprintf("https://%s/%s/playlist.m3u8", host, v.GUID)
			thumbFile := v.ThumbnailFileName
			if thumbFile == "" {
				thumbFile = "thumbnail.jpg"
			}
			body.Thumbnail = fmt.Sprintf("https://%s/%s/%s", host, v.GUID, thumbFile)
			body.Duration = int(v.Length)
			body.BunnyGUID = v.GUID
			if body.Title == "" {
				body.Title = v.Title
			}
		}
		body.VideoID = ""
	}

	// Resolve show slug for ID generation.
	showSlug := generateSlug(body.ShowID)
	if raw, _, err := s.doRest(ctx, "tv_shows", url.Values{"select": {"title"}, "id": {"eq." + url.QueryEscape(body.ShowID)}}); err == nil {
		var rows []struct{ Title string `json:"title"` }
		if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
			showSlug = generateSlug(rows[0].Title)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Resolve or create the season.
	var seasonID string
	switch {
	case body.SeasonID != "":
		// Season id supplied (e.g. the Firestore mirror passes a precomputed id).
		// If it already exists in Supabase, reuse it. Otherwise create it here —
		// the Firestore side may have created the season in Firestore only, and
		// blindly inserting the episode would violate the seasons FK (500).
		seasonID = body.SeasonID
		if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"id"}, "id": {"eq." + url.QueryEscape(seasonID)}}); err == nil {
			var rows []struct{ ID string `json:"id"` }
			if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
				// exists — reuse
				break
			}
		}
		title := body.SeasonTitle
		if title == "" {
			title = seasonID
		}
		var nextOrder int = 1
		if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"ord"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}, "order": {"ord.desc"}, "limit": {"1"}}); err == nil {
			var rows []struct{ Ord int `json:"ord"` }
			if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
				nextOrder = rows[0].Ord + 1
			}
		}
		seasonRow := map[string]any{
			"id": seasonID, "show_id": body.ShowID, "title": title,
			"ord": nextOrder, "episode_count": 0, "published": true,
			"created_at": now, "updated_at": now,
		}
		if err := s.restPostRow(ctx, "seasons", seasonRow); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	case body.SeasonTitle != "":
		var nextOrder int = 1
		if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"ord"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}, "order": {"ord.desc"}, "limit": {"1"}}); err == nil {
			var rows []struct{ Ord int `json:"ord"` }
			if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
				nextOrder = rows[0].Ord + 1
			}
		}
		seasonID = fmt.Sprintf("%s_s%02d", showSlug, nextOrder)
		seasonRow := map[string]any{
			"id": seasonID, "show_id": body.ShowID, "title": body.SeasonTitle,
			"ord": nextOrder, "episode_count": 0, "published": true,
			"created_at": now, "updated_at": now,
		}
		if err := s.restPostRow(ctx, "seasons", seasonRow); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	default:
		// Use the first season (lowest ord), else create "Season 1".
		if raw, _, err := s.doRest(ctx, "seasons", url.Values{"select": {"id"}, "show_id": {"eq." + url.QueryEscape(body.ShowID)}, "order": {"ord.asc"}, "limit": {"1"}}); err == nil {
			var rows []struct{ ID string `json:"id"` }
			if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
				seasonID = rows[0].ID
			}
		}
		if seasonID == "" {
			seasonID = fmt.Sprintf("%s_s01", showSlug)
			seasonRow := map[string]any{
				"id": seasonID, "show_id": body.ShowID, "title": "Season 1",
				"ord": 1, "episode_count": 0, "published": true,
				"created_at": now, "updated_at": now,
			}
			if err := s.restPostRow(ctx, "seasons", seasonRow); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
	}

	// Next episode number. IDs may not be contiguous (e.g. a mirror previously
	// created ep02 while ep01 is absent), so take the max existing _epNN suffix
	// + 1 instead of counting rows.
	episodeNum := 1
	if raw, _, err := s.doRest(ctx, "episodes", url.Values{"select": {"id"}, "season_id": {"eq." + url.QueryEscape(seasonID)}}); err == nil {
		var rows []struct{ ID string `json:"id"` }
		if json.Unmarshal(raw, &rows) == nil {
			maxNum := 0
			prefix := seasonID + "_ep"
			for _, r := range rows {
				if n, ok := parseEpisodeNum(r.ID, prefix); ok && n > maxNum {
					maxNum = n
				}
			}
			episodeNum = maxNum + 1
		}
	}
	episodeID := fmt.Sprintf("%s_ep%02d", seasonID, episodeNum)

	published := true
	if body.Published != nil {
		published = *body.Published
	}
	processing := false
	if body.Processing != nil {
		processing = *body.Processing
	}

	epRow := map[string]any{
		"id": episodeID, "season_id": seasonID, "show_id": body.ShowID,
		"title": body.Title, "description": body.Description,
		"duration": body.Duration, "thumbnail": orStr(body.Thumbnail, ""),
		"video_url": body.VideoURL, "sfx_job_name": orStr(body.SfxJobName, ""),
		"server": server,
		"bunny_guid": nilOrEmpty(body.BunnyGUID),
		"published": published, "processing": processing,
		"date_uploaded": now, "air_date": now,
		"created_at": now, "updated_at": now,
	}
	if err := s.restPostRow(ctx, "episodes", epRow); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Update season episodeCount.
	if raw, _, err := s.doRest(ctx, "episodes", url.Values{"select": {"id"}, "season_id": {"eq." + url.QueryEscape(seasonID)}}); err == nil {
		var rows []struct{ ID string `json:"id"` }
		if json.Unmarshal(raw, &rows) == nil {
			_ = s.restPatch(ctx, "seasons", "id=eq."+url.QueryEscape(seasonID), map[string]any{"episode_count": len(rows), "updated_at": now})
		}
	}

	s.clearOndemandCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"episode": map[string]any{
			"id": episodeID, "seasonId": seasonID, "showId": body.ShowID,
			"title": body.Title, "description": body.Description,
			"videoUrl": body.VideoURL, "thumbnail": body.Thumbnail,
			"duration": body.Duration, "server": server, "bunnyGuid": body.BunnyGUID,
			"seasonTitle": body.SeasonTitle,
		},
		"message": "Episode created successfully",
	})
}

// ──────────────── HELPERS ────────────────

// generateSlug mirrors the Firestore generateSlug helper.
func generateSlug(text string) string {
	re := regexp.MustCompile(`[^a-z0-9]+`)
	slug := re.ReplaceAllString(strings.ToLower(text), "_")
	slug = strings.Trim(slug, "_")
	if len(slug) > 50 {
		slug = slug[:50]
	}
	return slug
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// parseEpisodeNum extracts the numeric _epNN suffix from an episode id.
// e.g. parseEpisodeNum("ug_parliament_s02_ep02", "ug_parliament_s02_ep") = 2.
func parseEpisodeNum(id, prefix string) (int, bool) {
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	tail := strings.TrimPrefix(id, prefix)
	var n int
	if _, err := fmt.Sscanf(tail, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
