package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ──────────────── SUPABASE STORAGE (EPG images) ────────────────

// uploadBase64Image decodes a data:image base64 payload, uploads it to the
// Supabase Storage `epg-images` bucket under <station>/<orientation>/<name>,
// and returns the public URL. Mirrors uploadImageToStorage.
func (s *server) uploadBase64Image(ctx context.Context, base64Data, stationName, orientation, originalFileName string) (string, error) {
	if base64Data == "" || stationName == "" {
		return "", fmt.Errorf("base64 data and station name required")
	}
	// Parse mime + strip header.
	format := "jpeg"
	if m := regexp.MustCompile(`^data:image/([^;]+);base64,`).FindStringSubmatch(base64Data); len(m) == 2 {
		format = m[1]
	}
	clean := regexp.MustCompile(`^data:image/[^;]+;base64,`).ReplaceAllString(base64Data, "")
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", err
	}

	safeStation := regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(stationName, "_")
	safeStation = strings.ToLower(safeStation)

	fileName := fmt.Sprintf("%d", time.Now().UnixMilli())
	if originalFileName != "" {
		base := strings.TrimSuffix(filepath.Base(originalFileName), filepath.Ext(originalFileName))
		fileName = regexp.MustCompile(`[^a-zA-Z0-9_-]`).ReplaceAllString(base, "_")
	}
	if format == "svg+xml" {
		format = "svg+xml"
	}
	path := fmt.Sprintf("%s/%s/%s.%s", safeStation, orientation, fileName, format)
	mime := "image/" + format
	if format == "svg+xml" {
		mime = "image/svg+xml"
	}

	// Supabase Storage upload: POST /storage/v1/object/epg-images/<path>.
	// Use the internal URL for the upload (reachable in-cluster) but the public
	// URL for the stored link (browser-reachable).
	storageURL := s.restURL
	if i := strings.Index(storageURL, "/rest/v1"); i >= 0 {
		storageURL = storageURL[:i]
	}
	u := storageURL + "/storage/v1/object/epg-images/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Content-Type", mime)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("storage upload %d: %s", resp.StatusCode, string(body))
	}

	// Public URL via the mesh storage path (served by Supabase storage API).
	// Build from the public mesh origin, not the internal api-gw hostname.
	publicBase := getenv("PUBLIC_URL", "https://edge.solofx.net")
	publicURL := fmt.Sprintf("%s/storage/v1/object/public/epg-images/%s", publicBase, path)
	return publicURL, nil
}

// ──────────────── EPG PROGRAM CRUD ────────────────

type epgProgramWrite struct {
	ProgramName    string `json:"programName"`
	Presenter      any    `json:"presenter"`
	Genre          any    `json:"genre"`
	Details        any    `json:"details"`
	Language       any    `json:"language"`
	StartTime      string `json:"startTime"`
	EndTime        string `json:"endTime"`
	Days           any    `json:"days"`
	Type           any    `json:"type"`
	Image          any    `json:"image"`
	ImageLandscape any    `json:"imageLandscape"`
	Thumbnail      any    `json:"thumbnail"`
	TargetAudence  any    `json:"targetAudence"`
	TvProgramID    any    `json:"tvProgramId"`
	ImageFileName  string `json:"imageFileName"`
}

// handleAddEPGProgram mirrors addEPGProgram: inserts a program into a station.
// Body: { stationName, program: {...} }. Uploads base64 images if present.
func (s *server) handleAddEPGProgram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StationName string          `json:"stationName"`
		Program     epgProgramWrite `json:"program"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.StationName == "" || body.Program.ProgramName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stationName and program.programName are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Handle image uploads (data:image base64 → storage).
	if img, ok := body.Program.Image.(string); ok && strings.HasPrefix(img, "data:image") {
		u, err := s.uploadBase64Image(ctx, img, body.StationName, "portrait", body.Program.ImageFileName)
		if err != nil {
			log.Printf("epg image upload: %v", err)
		} else {
			body.Program.Image = u
		}
	}

	stationID := body.StationName
	if strings.EqualFold(stationID, "radio") {
		stationID = "Live_Radio"
	} else {
		stationID = s.resolveTVStation(ctx, body.StationName)
	}

	row := map[string]any{
		"station_id":     stationID,
		"program_name":   body.Program.ProgramName,
		"presenter":      nilOrJSON(body.Program.Presenter),
		"genre":          nilOrJSON(body.Program.Genre),
		"details":        nilOrJSON(body.Program.Details),
		"language":       nilOrJSON(body.Program.Language),
		"start_time":     body.Program.StartTime,
		"end_time":       body.Program.EndTime,
		"days":           nilOrJSON(body.Program.Days),
		"type":           nilOrJSON(body.Program.Type),
		"image":          nilOrJSON(body.Program.Image),
		"thumbnail":      nilOrJSON(body.Program.Thumbnail),
		"target_audience": nilOrJSON(body.Program.TargetAudence),
		"tv_program_id":  nilOrJSON(body.Program.TvProgramID),
		"updated_at":     time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.restPostRow(ctx, "epg_programs", row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.clearEPGCache()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Program added successfully",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// resolveTVStation validates the admin's stationName against epg_stations and
// returns the canonical id (e.g. "Salt TV One", "event", "Salt TV Two"). Falls
// back to the raw name if it isn't a known station.
func (s *server) resolveTVStation(ctx context.Context, name string) string {
	raw, _, err := s.doRest(ctx, "epg_stations", url.Values{"select": {"id"}, "id": {"eq." + url.QueryEscape(name)}})
	if err == nil {
		var rows []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 {
			return rows[0].ID
		}
	}
	return name
}

// handleUpdateEPGProgram mirrors updateEPGProgram: updates a program by
// tvProgramId or programName+days+startTime. Body: { stationName, programName,
// days, startTime, tvProgramId, updates: {...} }.
func (s *server) handleUpdateEPGProgram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StationName string `json:"stationName"`
		ProgramName string `json:"programName"`
		Days        string `json:"days"`
		StartTime   string `json:"startTime"`
		TvProgramID string `json:"tvProgramId"`
		Updates     map[string]any `json:"updates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.StationName == "" || body.Updates == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stationName and updates are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Build the SQL filter (by tvProgramId if provided, else name+days+start).
	filter := ""
	if body.TvProgramID != "" {
		filter = "tv_program_id=eq." + url.QueryEscape(body.TvProgramID)
	} else if body.ProgramName != "" && body.StartTime != "" {
		filter = "program_name=eq." + url.QueryEscape(body.ProgramName)
		if body.Days != "" {
			filter += "&days=eq." + url.QueryEscape(body.Days)
		}
		filter += "&start_time=eq." + url.QueryEscape(body.StartTime)
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tvProgramId or programName+days+startTime required"})
		return
	}

	// Upload images if provided.
	colMap := map[string]string{
		"programName": "program_name", "presenter": "presenter", "genre": "genre",
		"details": "details", "language": "language", "startTime": "start_time",
		"endTime": "end_time", "days": "days", "type": "type", "image": "image",
		"thumbnail": "thumbnail", "targetAudence": "target_audience", "tvProgramId": "tv_program_id",
	}
	row := map[string]any{}
	for k, v := range body.Updates {
		col, ok := colMap[k]
		if !ok {
			continue
		}
		if col == "image" || col == "thumbnail" {
			if img, ok := v.(string); ok && strings.HasPrefix(img, "data:image") {
				u, err := s.uploadBase64Image(ctx, img, body.StationName, "portrait", "")
				if err != nil {
					log.Printf("epg image upload: %v", err)
				} else {
					v = u
				}
			}
		}
		row[col] = nilOrJSON(v)
	}
	row["updated_at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.restPatch(ctx, "epg_programs", filter, row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.clearEPGCache()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Program updated successfully",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleDeleteEPGProgram mirrors deleteEPGProgram: deletes a program.
func (s *server) handleDeleteEPGProgram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StationName string `json:"stationName"`
		ProgramName string `json:"programName"`
		Days        string `json:"days"`
		StartTime   string `json:"startTime"`
		TvProgramID string `json:"tvProgramId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.StationName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stationName required"})
		return
	}

	filter := ""
	if body.TvProgramID != "" {
		filter = "tv_program_id=eq." + url.QueryEscape(body.TvProgramID)
	} else if body.ProgramName != "" && body.StartTime != "" {
		filter = "program_name=eq." + url.QueryEscape(body.ProgramName)
		if body.Days != "" {
			filter += "&days=eq." + url.QueryEscape(body.Days)
		}
		filter += "&start_time=eq." + url.QueryEscape(body.StartTime)
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tvProgramId or programName+days+startTime required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.restDelete(ctx, "epg_programs", filter); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.clearEPGCache()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Program deleted successfully",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}
