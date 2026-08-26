package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PostgREST write helpers used by the EPG/events writers. All use the
// service-role key (server-side).

// restPostRow inserts a single row (or row array) and returns the inserted row.
func (s *server) restPostRow(ctx context.Context, path string, row map[string]any) error {
	payload, _ := json.Marshal([]map[string]any{row})
	return s.restWrite(ctx, http.MethodPost, path, "", payload, "return=representation")
}

// restPatch updates rows matching the PostgREST filter (e.g. "id=eq.x").
func (s *server) restPatch(ctx context.Context, path, filter string, row map[string]any) error {
	payload, _ := json.Marshal(row)
	return s.restWrite(ctx, http.MethodPatch, path, filter, payload, "return=minimal")
}

// restPatchCount updates rows and returns how many rows actually matched.
// Used by update handlers so a 0-row patch (wrong filter / wrong seasonId)
// surfaces as an error instead of a silent success.
func (s *server) restPatchCount(ctx context.Context, path, filter string, row map[string]any) (int, error) {
	payload, _ := json.Marshal(row)
	body, _, err := s.restRead(ctx, http.MethodPatch, path, filter, payload, "return=representation")
	if err != nil {
		return 0, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// restDelete deletes rows matching the filter.
func (s *server) restDelete(ctx context.Context, path, filter string) error {
	return s.restWrite(ctx, http.MethodDelete, path, filter, nil, "return=minimal")
}

func (s *server) restWrite(ctx context.Context, method, path, filter string, payload []byte, prefer string) error {
	_, _, err := s.restRead(ctx, method, path, filter, payload, prefer)
	return err
}

// restRead performs a PostgREST request and returns the response body + headers.
func (s *server) restRead(ctx context.Context, method, path, filter string, payload []byte, prefer string) ([]byte, http.Header, error) {
	u := s.restURL + "/" + path
	if filter != "" {
		u += "?" + filter
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, resp.Header, nil
}

// nilOrJSON returns nil for nil input, otherwise passes the value through
// (PostgREST encodes as JSON; strings/numbers/bools arrive as-is).
func nilOrJSON(v any) any {
	if v == nil {
		return nil
	}
	return v
}
