package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const bqScope = "https://www.googleapis.com/auth/bigquery"

// bigQueryQuery runs a BigQuery query via the REST API using the FCM service
// account (firebase-adminsdk on salt-media-app1 — verified to have BigQuery
// read access). Returns rows as maps keyed by column name. Only used for small
// pre-aggregated results (the heavy lifting happens inside BigQuery).
func (s *server) bigQueryQuery(ctx context.Context, query string) ([]map[string]any, error) {
	sa, err := loadFCMSA()
	if err != nil {
		return nil, err
	}
	token, err := s.serviceAccountToken(ctx, sa, bqScope)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"query":        query,
		"useLegacySql": false,
		"timeoutMs":    120000,
		"maxResults":   10000,
	})
	u := "https://bigquery.googleapis.com/bigquery/v2/projects/" + sa.ProjectID + "/queries"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bigquery %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Schema struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"schema"`
		Rows []struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		} `json:"rows"`
		JobComplete bool `json:"jobComplete"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if !out.JobComplete {
		return nil, fmt.Errorf("bigquery job not complete")
	}
	names := make([]string, len(out.Schema.Fields))
	for i, f := range out.Schema.Fields {
		names[i] = f.Name
	}
	rows := make([]map[string]any, 0, len(out.Rows))
	for _, r := range out.Rows {
		m := map[string]any{}
		for i, cell := range r.F {
			if i < len(names) {
				m[names[i]] = cell.V
			}
		}
		rows = append(rows, m)
	}
	return rows, nil
}
