package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRadioIngestBuffersAndDeduplicates(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	s := &server{outbox: ob}
	body := `{"station_id":307,"records":[{"played_at":"2026-10-29 06:00:00","track_type":"Spot","client":"MTN","title":"MTN Test","duration_seconds":30,"filename":"F:\\Adverts\\MTN.mp3"}]}`

	call := func() (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingestTrafficLogs", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleIngestTrafficLogs(rec, req)
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return rec, response
	}

	firstRec, first := call()
	if firstRec.Code != http.StatusOK || first["queued"] != float64(1) || first["buffered"] != true {
		t.Fatalf("first = %d %#v", firstRec.Code, first)
	}
	secondRec, second := call()
	if secondRec.Code != http.StatusOK || second["duplicate"] != true {
		t.Fatalf("second = %d %#v", secondRec.Code, second)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestRadioWebhookBuffers(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	s := &server{outbox: ob}
	url := "/api/v1/stationPlaylistWebhook?station=307&file=F%3A%5CAdverts%5CMTN.mp3&title=MTN%20Test&cat=Spot&dur=00%3A30&date=2026-10-29&time=06%3A00%3A00&client=MTN"

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	s.handleStationPlaylistWebhook(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "OK" {
		t.Fatalf("webhook = %d %q", rec.Code, rec.Body.String())
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestContentAnalyticsBuffersAndDeduplicates(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	s := &server{outbox: ob}
	body := `{"events":[{"content_id":"c1","content_type":"movie","user_id":"u1","timestamp":1790000000000}]}`

	call := func() (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/batchTrackContentViews", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.trackView(rec, req)
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return rec, response
	}

	firstRec, first := call()
	if firstRec.Code != http.StatusOK || first["queued"] != float64(1) || first["buffered"] != true {
		t.Fatalf("first = %d %#v", firstRec.Code, first)
	}
	_, second := call()
	if second["duplicate"] != true {
		t.Fatalf("second = %#v, want duplicate", second)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestAdEventsBuffer(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	s := &server{outbox: ob}
	body := `{"events":[{"ad_id":"ad1","event_type":"impression","user_id":"u1","timestamp":1790000000000,"metadata":{"placement":"preroll"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batchTrackAdEvents", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleBatchTrackAdEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response["queued"] != float64(1) || response["eventsProcessed"] != float64(1) {
		t.Fatalf("response = %#v", response)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestVarnishSnapshotBuffers(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 0, DedupeTTL: time.Hour})
	s := &server{outbox: ob}
	if err := s.persistVarnishSnapshot(context.Background()); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestNormalizeAnalyticsTimes(t *testing.T) {
	rows := []map[string]any{{
		"timestamp":   "2026-10-29T06:00:00Z",
		"received_at": "2026-10-29T06:00:01Z",
		"other":       "unchanged",
	}}
	normalizeAnalyticsTimes(rows)
	if _, ok := rows[0]["timestamp"].(time.Time); !ok {
		t.Fatalf("timestamp = %T, want time.Time", rows[0]["timestamp"])
	}
	if _, ok := rows[0]["received_at"].(time.Time); !ok {
		t.Fatalf("received_at = %T, want time.Time", rows[0]["received_at"])
	}
	if rows[0]["other"] != "unchanged" {
		t.Fatalf("other = %v", rows[0]["other"])
	}
}
