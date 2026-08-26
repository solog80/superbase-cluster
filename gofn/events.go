package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Events CRUD — mirrors epg.js getEvents/addEvent/updateEvent/deleteEvent.
// Data lives in the Supabase `events` table. Active-event cache is in-memory.

// handleGetEvents returns all events ordered by startDate desc.
func (s *server) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, _, err := s.doRest(ctx, "events", url.Values{"select": {"*"}, "order": {"start_date.desc"}})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "bad response"})
		return
	}
	// Convert snake_case → camelCase to match the old Firestore shape.
	events := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		events = append(events, map[string]any{
			"id":         row["id"],
			"title":      row["title"],
			"imageUrl":   row["image_url"],
			"presenter":  row["presenter"],
			"startDate":  row["start_date"],
			"endDate":    row["end_date"],
			"platform":   row["platform"],
			"stations":   row["stations"],
			"createdAt":  row["created_at"],
			"updatedAt":  row["updated_at"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleAddEvent mirrors addEvent: creates an event and refreshes the active
// cache.
func (s *server) handleAddEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string   `json:"title"`
		ImageURL  string   `json:"imageUrl"`
		Presenter string   `json:"presenter"`
		StartDate string   `json:"startDate"`
		EndDate   string   `json:"endDate"`
		Platform  string   `json:"platform"`
		Stations  []string `json:"stations"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.Title == "" || body.StartDate == "" || body.EndDate == "" || body.Platform == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title, startDate, endDate, and platform are required"})
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	row := map[string]any{
		"title":      body.Title,
		"image_url":  body.ImageURL,
		"presenter":  body.Presenter,
		"start_date": body.StartDate,
		"end_date":   body.EndDate,
		"platform":   body.Platform,
		"stations":   body.Stations,
		"created_at": now,
		"updated_at": now,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.restPostRow(ctx, "events", row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleUpdateEvent mirrors updateEvent: updates an event by id.
func (s *server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID   string   `json:"eventId"`
		Title     *string  `json:"title"`
		ImageURL  *string  `json:"imageUrl"`
		Presenter *string  `json:"presenter"`
		StartDate *string  `json:"startDate"`
		EndDate   *string  `json:"endDate"`
		Platform  *string  `json:"platform"`
		Stations  []string `json:"stations"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.EventID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "eventId is required"})
		return
	}

	row := map[string]any{}
	if body.Title != nil {
		row["title"] = *body.Title
	}
	if body.ImageURL != nil {
		row["image_url"] = *body.ImageURL
	}
	if body.Presenter != nil {
		row["presenter"] = *body.Presenter
	}
	if body.StartDate != nil {
		row["start_date"] = *body.StartDate
	}
	if body.EndDate != nil {
		row["end_date"] = *body.EndDate
	}
	if body.Platform != nil {
		row["platform"] = *body.Platform
	}
	if body.Stations != nil {
		row["stations"] = body.Stations
	}
	row["updated_at"] = time.Now().UTC().Format(time.RFC3339)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.restPatch(ctx, "events", "id=eq."+url.QueryEscape(body.EventID), row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleDeleteEvent mirrors deleteEvent: deletes an event by id.
func (s *server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID string `json:"eventId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.EventID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "eventId is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.restDelete(ctx, "events", "id=eq."+url.QueryEscape(body.EventID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
