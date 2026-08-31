package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Joomla article CRUD + reference, proxied to the Joomla REST API from this
// mesh server (Edge). The origin accepts these server-side calls with the
// saltapi Basic auth (env: JOOMLA_API_URL/USERNAME/PASSWORD). This is the
// same path the access logs show working (Go-http-client from Edge).
//
// JOOMLA_API_URL already includes the API base (e.g.
// https://saltmedia.ug/api/index.php/v1); paths are appended directly.

type joomlaClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

func newJoomlaClient() *joomlaClient {
	// Force IPv4: the container's DNS resolves saltmedia.ug to an IPv6 AAAA
	// (2606:...) that hangs from the bridge network; the origin is IPv4.
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	return &joomlaClient{
		baseURL:  strings.TrimSuffix(os.Getenv("JOOMLA_API_URL"), "/"),
		username: os.Getenv("JOOMLA_API_USERNAME"),
		password: os.Getenv("JOOMLA_API_PASSWORD"),
		http:     &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}
}

// request performs a Joomla API call and returns the parsed JSON body.
// path is relative to the base URL (e.g. "content/articles").
func (j *joomlaClient) request(ctx context.Context, method, path string, params url.Values, payload any) (json.RawMessage, int, error) {
	u := j.baseURL + "/" + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(j.username, j.password)
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// handleGetNewsArticles: Joomla article list (JSON:API passthrough).
// When a `search` term is present, queries OpenSearch (fuzzy, fast) and maps
// results back to the JSON:API shape the dashboard parses. Otherwise proxies
// the Joomla API (list/pagination). Falls back to the Joomla API if OpenSearch
// is unavailable.
func (s *server) handleGetNewsArticles(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	limit, _ := strconv.Atoi(qv.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// Search path: OpenSearch fuzzy on title/body/category.
	if qv.Get("search") != "" && s.osClient != nil {
		rows, total, err := s.osSearchNewsArticles(r.Context(), qv.Get("search"), qv.Get("category"), limit, offset)
		if err == nil {
			data := make([]map[string]any, 0, len(rows))
			for _, src := range rows {
				attributes := map[string]any{
					"id":         src["id"],
					"title":      src["title"],
					"alias":      src["alias"],
					"state":      src["state"],
					"hits":       src["hits"],
					"featured":   src["featured"],
					"created":    src["created"],
					"publish_up": src["publish_up"],
					"images":     src["images"],
					"text":       src["body"],
				}
				if alias, ok := src["created_by_alias"]; ok && alias != nil {
					attributes["created_by_alias"] = fmt.Sprint(alias)
				}
				relationships := map[string]any{
					"category": map[string]any{"data": map[string]any{"type": "categories", "id": src["category"]}},
				}
				if cb, ok := src["created_by"]; ok && cb != nil && fmt.Sprint(cb) != "" {
					relationships["created_by"] = map[string]any{"data": map[string]any{"type": "users", "id": fmt.Sprint(cb)}}
				}
				data = append(data, map[string]any{
					"type":          "articles",
					"id":            fmt.Sprint(src["id"]),
					"attributes":    attributes,
					"relationships": relationships,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"total": total, "limit": limit, "offset": offset}})
			return
		}
		log.Printf("opensearch news search failed (%v); falling back to Joomla API", err)
	}

	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	params := url.Values{}
	params.Set("page[limit]", firstNonEmpty(qv.Get("limit"), "20"))
	params.Set("page[offset]", firstNonEmpty(qv.Get("offset"), "0"))
	if v := qv.Get("search"); v != "" {
		params.Set("filter[search]", v)
	}
	if v := qv.Get("category"); v != "" {
		params.Set("filter[category]", v)
	}
	if v := qv.Get("state"); v != "" {
		params.Set("filter[state]", v)
	}
	if v := qv.Get("featured"); v != "" {
		params.Set("filter[featured]", v)
	}
	if v := qv.Get("author"); v != "" {
		params.Set("filter[author]", v)
	}
	params.Set("sort", "-created")

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, status, err := j.request(ctx, http.MethodGet, "content/articles", params, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeRawJSON(w, status, body)
}

// osSearchNewsArticles queries OpenSearch saltmedia-joomla_articles for the
// news dashboard search (fuzzy multi-field on title/body, optional category).
func (s *server) osSearchNewsArticles(ctx context.Context, q, cat string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	if cat != "" {
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": q, "fields": []string{"title", "body"}, "fuzziness": "AUTO"}},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"category": cat}},
				},
			},
		}
	} else {
		query = map[string]any{
			"multi_match": map[string]any{"query": q, "fields": []string{"title", "body"}, "fuzziness": "AUTO"},
		}
	}
	body, _ := json.Marshal(map[string]any{
		"query": query,
		"from":  offset,
		"size":  limit,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.osURL+"/saltmedia-joomla_articles/_search", strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.osUser, s.osPass)
	resp, err := s.osClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("os HTTP %d", resp.StatusCode)
	}
	var out struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, 0, err
	}
	rows := make([]map[string]any, 0, len(out.Hits.Hits))
	for _, h := range out.Hits.Hits {
		rows = append(rows, h.Source)
	}
	return rows, out.Hits.Total.Value, nil
}

// handleGetNewsArticle: single article (JSON:API passthrough).
func (s *server) handleGetNewsArticle(w http.ResponseWriter, r *http.Request) {
	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, status, err := j.request(ctx, http.MethodGet, "content/articles/"+url.PathEscape(id), nil, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeRawJSON(w, status, body)
}

// handleCreateJoomlaArticle: create article (JSON:API passthrough).
func (s *server) handleCreateJoomlaArticle(w http.ResponseWriter, r *http.Request) {
	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, status, err := j.request(ctx, http.MethodPost, "content/articles", nil, payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeRawJSON(w, http.StatusCreated, body)
}

// handleUpdateJoomlaArticle: update article (JSON:API passthrough).
func (s *server) handleUpdateJoomlaArticle(w http.ResponseWriter, r *http.Request) {
	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, status, err := j.request(ctx, http.MethodPatch, "content/articles/"+url.PathEscape(id), nil, payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeRawJSON(w, status, body)
}

// handleDeleteJoomlaArticle: trash (state=-2) then delete article.
func (s *server) handleDeleteJoomlaArticle(w http.ResponseWriter, r *http.Request) {
	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// Joomla requires trashing (state=-2) before delete.
	if _, status, err := j.request(ctx, http.MethodPatch, "content/articles/"+url.PathEscape(id), nil, map[string]any{"state": -2}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	} else if status >= 300 {
		writeJSON(w, status, jsonOrError(nil, status))
		return
	}

	body, status, err := j.request(ctx, http.MethodDelete, "content/articles/"+url.PathEscape(id), nil, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 && status != 204 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleGetJoomlaReference: categories/authors/tags (JSON:API passthrough).
func (s *server) handleGetJoomlaReference(w http.ResponseWriter, r *http.Request) {
	j := newJoomlaClient()
	if j.username == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "joomla api not configured"})
		return
	}
	typ := r.URL.Query().Get("type")
	if typ == "" {
		typ = "categories"
	}
	var path string
	switch typ {
	case "categories":
		path = "content/categories"
	case "authors":
		path = "users"
	case "tags":
		path = "tags"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown type: " + typ})
		return
	}
	params := url.Values{}
	params.Set("page[limit]", "200")
	if typ == "categories" {
		params.Set("filter[extension]", "com_content")
		params.Set("sort", "title")
	} else {
		params.Set("sort", "name")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, status, err := j.request(ctx, http.MethodGet, path, params, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if status >= 300 {
		writeJSON(w, status, jsonOrError(body, status))
		return
	}
	writeRawJSON(w, status, body)
}

func jsonOrError(body []byte, status int) map[string]any {
	var m map[string]any
	if len(body) > 0 && json.Unmarshal(body, &m) == nil {
		return m
	}
	return map[string]any{"error": fmt.Sprintf("HTTP %d", status)}
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
