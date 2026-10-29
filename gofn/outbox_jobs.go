package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	outboxKindVideoIngest     = "video.ingest"
	outboxKindRadioIngest     = "radio.ingest"
	outboxKindAnalyticsRows   = "analytics.rows"
	outboxKindVarnishSnapshot = "varnish.snapshot"
)

type videoIngestJob struct {
	ChannelID   int                  `json:"channel_id"`
	ChannelName string               `json:"channel_name"`
	Records     []VideoTrafficRecord `json:"records"`
	ReceivedAt  time.Time            `json:"received_at"`
}

type radioIngestJob struct {
	StationID  int             `json:"station_id"`
	Records    []TrafficRecord `json:"records"`
	ReceivedAt time.Time       `json:"received_at"`
}

type analyticsRowsJob struct {
	Table      string           `json:"table"`
	Rows       []map[string]any `json:"rows"`
	ReceivedAt time.Time        `json:"received_at"`
}

type varnishSnapshotJob struct {
	Timestamp  time.Time `json:"timestamp"`
	Stream1    int       `json:"stream1"`
	Stream2    int       `json:"stream2"`
	ReceivedAt time.Time `json:"received_at"`
}

// handleOutboxJob dispatches one durable buffered write. Jobs are retried by
// the outbox; only permanent errors (bad payload / unknown kind) go straight to
// the dead-letter bucket.
func (s *server) handleOutboxJob(ctx context.Context, job outboxJob) error {
	switch job.Kind {
	case outboxKindVideoIngest:
		var payload videoIngestJob
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return permanentJob(fmt.Errorf("decode %s payload: %w", job.Kind, err))
		}
		if payload.ChannelID == 0 {
			payload.ChannelID = 401
		}
		if len(payload.Records) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		_, err := s.insertVideoTrafficRecords(ctx, payload.ChannelID, payload.ChannelName, payload.Records)
		return err

	case outboxKindRadioIngest:
		var payload radioIngestJob
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return permanentJob(fmt.Errorf("decode %s payload: %w", job.Kind, err))
		}
		if payload.StationID == 0 {
			payload.StationID = 307
		}
		if len(payload.Records) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		_, err := s.insertTrafficRecords(ctx, payload.StationID, payload.Records)
		return err

	case outboxKindAnalyticsRows:
		var payload analyticsRowsJob
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return permanentJob(fmt.Errorf("decode %s payload: %w", job.Kind, err))
		}
		if !allowedAnalyticsTable(payload.Table) {
			return permanentJob(fmt.Errorf("unsupported analytics table %q", payload.Table))
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return s.insertRowsJSON(ctx, payload.Table, payload.Rows)

	case outboxKindVarnishSnapshot:
		var payload varnishSnapshotJob
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return permanentJob(fmt.Errorf("decode %s payload: %w", job.Kind, err))
		}
		if payload.Timestamp.IsZero() {
			return permanentJob(fmt.Errorf("%s payload has no timestamp", job.Kind))
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return s.insertVarnishSnapshot(ctx, payload.Timestamp, payload.Stream1, payload.Stream2)

	default:
		return permanentJob(fmt.Errorf("unknown outbox job kind %q", job.Kind))
	}
}

// handleReplayOutboxDead moves dead-lettered jobs back to pending. Gated to the
// service role key; intended for post-upgrade recovery, not routine traffic.
func (s *server) handleReplayOutboxDead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	if s.outbox == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "outbox disabled"})
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 1000)
	moved, err := s.outbox.ReplayDead(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"replayed":       moved,
		"remaining_dead": s.outbox.Stats().Dead,
	})
}
