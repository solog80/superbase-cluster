package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIngestVideoTrafficBuffersAndDeduplicates(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 3, DedupeTTL: time.Hour})
	s := &server{outbox: ob}

	body := `{
		"channel_id": 401,
		"channel_name": "Salt TV One",
		"records": [{
			"played_at": "2026-10-29 06:00:00",
			"channel_id": 401,
			"monitor": "Program",
			"client": "MTN",
			"title": "MTN Test",
			"duration_seconds": 30,
			"filename": "D:\\Ads\\MTN Test.mp4",
			"input_number": 1,
			"input_key": "abc",
			"log_hash": "hash-1"
		}]
	}`

	call := func() (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingestVideoTrafficLogs", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleIngestVideoTrafficLogs(rec, req)
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
		}
		return rec, response
	}

	firstRec, first := call()
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", firstRec.Code)
	}
	if first["queued"] != float64(1) || first["buffered"] != true || first["duplicate"] != false {
		t.Fatalf("first response = %#v", first)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}

	secondRec, second := call()
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", secondRec.Code)
	}
	if second["duplicate"] != true {
		t.Fatalf("second response = %#v, want duplicate=true", second)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending after duplicate = %d, want 1", stats.Pending)
	}
}

func TestUploadVideoTrafficBuffers(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxAttempts: 3, DedupeTTL: time.Hour})
	s := &server{outbox: ob}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "VLog-261029.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	csvText := "played_at,channel_id,monitor,client,title,duration_seconds,filename,input_number,input_key,log_hash\n" +
		"2026-10-29 06:00:00,401,Program,MTN,MTN Test,30,D:\\Ads\\MTN Test.mp4,1,abc,hash-1\n"
	if _, err := part.Write([]byte(csvText)); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploadVideoTrafficLogFile?channel=401", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	s.handleUploadVideoTrafficLogFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["parsed"] != float64(1) || response["queued"] != float64(1) || response["buffered"] != true {
		t.Fatalf("response = %#v", response)
	}
	if stats := ob.Stats(); stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
}

func TestIngestVideoTrafficReturns503WhenBufferFull(t *testing.T) {
	ob := testOutbox(t, outboxConfig{MaxBytes: 1, MaxAttempts: 3, DedupeTTL: time.Hour})
	s := &server{outbox: ob}

	body := `{"channel_id":401,"records":[{"played_at":"2026-10-29 06:00:00","title":"x","filename":"x.mp4","log_hash":"h"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingestVideoTrafficLogs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleIngestVideoTrafficLogs(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
}
