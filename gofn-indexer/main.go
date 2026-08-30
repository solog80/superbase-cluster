// salt-gofn-indexer: catalog search indexer for the Salt TV mesh.
//
// Reads catalog data from the geo-routed READ REPLICAS (never the write
// primary) over the Tailscale mesh and indexes it into OpenSearch on
// srt-node. A search index is a derived cache, so it must keep working even
// with the primary down.
//
// Index layout: <index-prefix>-<table> per table (e.g. saltmedia-tv_shows).
// Run modes:
//   - once:   do a single full re-index and exit (used by cron)
//   - watch:  stay running and re-index on an interval
//
// Usage:
//
//	INDEXER_MODE=once ./salt-gofn-indexer
//	INDEXER_MODE=watch INDEXER_INTERVAL=300 ./salt-gofn-indexer
//
// Env:
//
//		OPENSEARCH_URL          e.g. https://100.127.244.33:9200
//		OPENSEARCH_USER         default admin
//		OPENSEARCH_PASSWORD     admin password
//		OPENSEARCH_INSECURE     set "1" to skip TLS verify (self-signed)
//		ANON_KEY                Supabase anon key (replica reads)
//		INDEX_PREFIX            default saltmedia
//	  REPLICAS                comma-separated read endpoint base URLs (failover order),
//	                          each including its path prefix:
//	                            us2:  http://100.82.159.75:5557
//	                            eu1:  http://100.116.100.32:5557/rest/v1
//	                            eu2:  http://100.99.30.100:5557/rest/v1
//	  INDEXER_MODE            once | watch (default once)
//	  INDEXER_INTERVAL        seconds between watch re-indexes (default 300)
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type indexer struct {
	osURL    string
	osUser   string
	osPass   string
	client   *http.Client
	prefix   string
	replicas []string
	anonKey  string

	meshBase string // mesh base URL for the Joomla article feed (e.g. https://edge.solofx.net)
	meshKey  string // mesh service key for the feed call
	insecure bool   // skip TLS verify (self-signed/expired origin certs)
}

// indexDef describes one searchable collection.
type indexDef struct {
	index   string
	table   string
	selects string
	filter  string // optional PostgREST filter, e.g. "name=not.is.null"
}

var indices = []indexDef{
	{index: "tv_shows", table: "tv_shows", selects: "id,title,description,type,thumbnail,poster_url_2x3,poster_url_16x9,season_count,published,updated_at"},
	{index: "epg_programs", table: "epg_programs", selects: "id,station_id,program_name,presenter,genre,details,language,start_time,end_time,days,type,image,thumbnail"},
	{index: "events", table: "events", selects: "*"},
	{index: "ads", table: "ads", selects: "*"},
	{index: "users", table: "users", selects: "id,name,email,user_name,profile_image_url,role,is_admin,is_blocked,is_verified,is_anonymous,created_at,updated_at", filter: "name=not.is.null"},
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	mode := os.Getenv("INDEXER_MODE")
	if mode == "" {
		mode = "once"
	}
	interval := 300
	if v := os.Getenv("INDEXER_INTERVAL"); v != "" {
		fmt.Sscanf(v, "%d", &interval)
	}

	ix := newIndexer()
	log.Printf("indexer starting (mode=%s, replicas=%d, prefix=%s)", mode, len(ix.replicas), ix.prefix)

	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := ix.runFull(ctx); err != nil {
			log.Printf("FULL INDEX FAILED: %v", err)
		}
	}

	switch mode {
	case "watch":
		run()
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			run()
		}
	default:
		run()
	}
}

func newIndexer() *indexer {
	insecure := os.Getenv("OPENSEARCH_INSECURE") == "1"
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	replicas := []string{}
	for _, r := range strings.Split(os.Getenv("REPLICAS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			replicas = append(replicas, r)
		}
	}
	if len(replicas) == 0 {
		replicas = []string{"http://100.82.159.75:5557"} // default: us2 bare-path replica
	}
	ix := &indexer{
		osURL:    strings.TrimSuffix(os.Getenv("OPENSEARCH_URL"), "/"),
		osUser:   getenv("OPENSEARCH_USER", "admin"),
		osPass:   os.Getenv("OPENSEARCH_PASSWORD"),
		client:   &http.Client{Transport: tr, Timeout: 15 * time.Second},
		prefix:   getenv("INDEX_PREFIX", "saltmedia"),
		replicas: replicas,
		anonKey:  os.Getenv("ANON_KEY"),
		meshBase: strings.TrimSuffix(os.Getenv("MESH_BASE_URL"), "/"),
		meshKey:  os.Getenv("MESH_SERVICE_KEY"),
		insecure: os.Getenv("OPENSEARCH_INSECURE") == "1",
	}
	if ix.osURL == "" {
		log.Fatal("OPENSEARCH_URL required")
	}
	if ix.osPass == "" {
		log.Fatal("OPENSEARCH_PASSWORD required")
	}
	if ix.anonKey == "" {
		log.Fatal("ANON_KEY required")
	}
	return ix
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// replicaRead fetches a full table from the first replica that answers.
// Fails over across all configured replicas (primary is deliberately NOT
// included - replicas only).
// Each replica base URL must include its full path prefix, e.g.:
//   - us2:  http://100.82.159.75:5557            (bare paths)
//   - eu1:  http://100.116.100.32:5557/rest/v1   (nginx strips /rest/v1)
//   - eu2:  http://100.99.30.100:5557/rest/v1
//
// The table is appended directly to the base URL. Fetches ALL rows in pages
// of 1000 (reads Content-Range for the total when the replica returns it).
func (ix *indexer) replicaRead(ctx context.Context, table, sel, filter string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	var lastErr error
	for _, base := range ix.replicas {
		all = nil
		offset := 0
		for {
			u := fmt.Sprintf("%s/%s?select=%s&limit=1000&offset=%d", base, table, urlQueryEscape(sel), offset)
			if filter != "" {
				u += "&" + filter
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				lastErr = err
				break
			}
			req.Header.Set("apikey", ix.anonKey)
			req.Header.Set("Authorization", "Bearer "+ix.anonKey)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Prefer", "count=exact")
			req.Header.Set("Range-Unit", "items")
			resp, err := ix.client.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", base, err)
				log.Printf("  replica %s failed (%v); trying next", base, err)
				all = nil
				break
			}
			body, _ := io.ReadAll(resp.Body)
			cr := resp.Header.Get("Content-Range")
			resp.Body.Close()
			if resp.StatusCode != 200 && resp.StatusCode != 206 {
				lastErr = fmt.Errorf("%s: HTTP %d", base, resp.StatusCode)
				log.Printf("  replica %s HTTP %d; trying next", base, resp.StatusCode)
				all = nil
				break
			}
			var rows []json.RawMessage
			if err := json.Unmarshal(body, &rows); err != nil {
				lastErr = fmt.Errorf("%s: decode: %w", base, err)
				all = nil
				break
			}
			all = append(all, rows...)
			if len(rows) < 1000 {
				log.Printf("  read %d rows of %s from %s", len(all), table, base)
				return all, nil
			}
			// Parse total from "start-end/total" to know when we're done.
			total := 0
			fmt.Sscanf(cr, "%d-%d/%d", new(int), new(int), &total)
			offset += len(rows)
			if total > 0 && offset >= total {
				log.Printf("  read %d rows of %s from %s", len(all), table, base)
				return all, nil
			}
		}
	}
	return nil, fmt.Errorf("all replicas failed: %w", lastErr)
}

// runFull re-indexes every collection, deleting and rebuilding the index
// (idempotent full sync). A search index is a cache; a full rebuild per run
// is the simplest correct approach at this scale.
func (ix *indexer) runFull(ctx context.Context) error {
	start := time.Now()
	total := 0
	for _, def := range indices {
		idx := ix.prefix + "-" + def.index
		rows, err := ix.replicaRead(ctx, def.table, def.selects, def.filter)
		if err != nil {
			return fmt.Errorf("%s: %w", def.table, err)
		}
		if err := ix.rebuildIndex(ctx, idx, rows); err != nil {
			return fmt.Errorf("index %s: %w", idx, err)
		}
		total += len(rows)
		log.Printf("  indexed %s (%d docs)", idx, len(rows))
	}

	// Joomla articles (site content) — pulled via the mesh (Joomla API proxy).
	if ix.meshBase != "" {
		n, err := ix.indexJoomla(ctx)
		if err != nil {
			return fmt.Errorf("joomla: %w", err)
		}
		total += n
	}

	log.Printf("FULL INDEX OK: %d docs across %d indices in %s", total, len(indices), time.Since(start).Round(time.Millisecond))
	return nil
}

// indexJoomla pulls Joomla articles from the MESH getNewsArticles endpoint
// (which proxies the Joomla API from Edge — the reliable path) and bulk-loads
// them into <prefix>-joomla_articles. Paginates page[limit]=500.
//
// This keeps a SINGLE source of truth: the Joomla API (same source the
// dashboard reads/writes), not a separate MySQL export.
func (ix *indexer) indexJoomla(ctx context.Context) (int, error) {
	idx := ix.prefix + "-joomla_articles"

	jc := &http.Client{Timeout: 120 * time.Second}
	var rows []json.RawMessage
	offset := 0
	const pageSize = 500
	for {
		u := fmt.Sprintf("%s/api/v1/getNewsArticles?limit=%d&offset=%d&state=1", ix.meshBase, pageSize, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("apikey", ix.meshKey)
		req.Header.Set("Authorization", "Bearer "+ix.meshKey)
		resp, err := jc.Do(req)
		if err != nil {
			return 0, err
		}
		var out struct {
			Data []map[string]any `json:"data"`
		}
		dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
		derr := dec.Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("mesh getNewsArticles HTTP %d", resp.StatusCode)
		}
		if derr != nil {
			return 0, derr
		}
		if len(out.Data) == 0 {
			break
		}
		for _, item := range out.Data {
			// JSON:API item -> flat OpenSearch doc.
			attrs, _ := item["attributes"].(map[string]any)
			// Joomla returns 0/1 for featured/state; coerce to bool for the
			// OpenSearch boolean mapping.
			featured := toBool(attrs["featured"])
			doc := map[string]any{
				"id":         attrs["id"],
				"title":      attrs["title"],
				"alias":      attrs["alias"],
				"category":   joomlaCategory(item),
				"created":    attrs["created"],
				"publish_up": attrs["publish_up"],
				"state":      attrs["state"],
				"hits":       attrs["hits"],
				"featured":   featured,
				"created_by": attrs["created_by"],
				"images":     attrs["images"],
				"metadesc":   attrs["metadesc"],
				"body":       attrs["text"],
			}
			b, _ := json.Marshal(doc)
			rows = append(rows, b)
		}
		offset += len(out.Data)
		if len(out.Data) < pageSize {
			break
		}
	}

	if err := ix.rebuildIndex(ctx, idx, rows); err != nil {
		return 0, err
	}
	log.Printf("  indexed %s (%d docs)", idx, len(rows))
	return len(rows), nil
}

// joomlaCategory extracts the category from a JSON:API item's relationships
// (may be missing in list responses; falls back to empty).
func joomlaCategory(item map[string]any) string {
	rel, _ := item["relationships"].(map[string]any)
	cat, _ := rel["category"].(map[string]any)
	data, _ := cat["data"].(map[string]any)
	id, _ := data["id"].(string)
	return id
}

// toBool coerces Joomla's 0/1/"0"/"1"/bool into a Go bool for OpenSearch.
func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || t == "true"
	}
	return false
}

// rebuildIndex deletes + recreates the index and bulk-loads docs.
func (ix *indexer) rebuildIndex(ctx context.Context, index string, rows []json.RawMessage) error {
	del := fmt.Sprintf("%s/%s", ix.osURL, index)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, del, nil)
	ix.setAuth(req)
	resp, err := ix.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// 404 = fine (no index yet). Other errors logged but we continue to create.

	// Create index (idempotent).
	mapping := fmt.Sprintf(`{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":{"properties":{"title":{"type":"text"},"program_name":{"type":"text"},"description":{"type":"text"},"genre":{"type":"keyword"},"type":{"type":"keyword"},"station_id":{"type":"keyword"},"published":{"type":"boolean"},"name":{"type":"text"},"email":{"type":"keyword"},"user_name":{"type":"text"},"role":{"type":"keyword"},"is_admin":{"type":"boolean"},"is_blocked":{"type":"boolean"},"body":{"type":"text"},"category":{"type":"keyword"},"featured":{"type":"boolean"}}}}`)
	creq, _ := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/%s", ix.osURL, index), bytes.NewBufferString(mapping))
	creq.Header.Set("Content-Type", "application/json")
	ix.setAuth(creq)
	cresp, err := ix.client.Do(creq)
	if err != nil {
		return err
	}
	cbody, _ := io.ReadAll(cresp.Body)
	cresp.Body.Close()
	if cresp.StatusCode >= 300 && cresp.StatusCode != 400 {
		return fmt.Errorf("create index: HTTP %d %s", cresp.StatusCode, strings.TrimSpace(string(cbody)))
	}

	if len(rows) == 0 {
		return nil
	}

	// Bulk load: two lines per doc (action + source).
	var buf bytes.Buffer
	for _, row := range rows {
		var doc map[string]any
		if err := json.Unmarshal(row, &doc); err != nil {
			continue
		}
		id := fmt.Sprint(doc["id"])
		buf.WriteString(fmt.Sprintf(`{"index":{"_index":"%s","_id":"%s"}}`+"\n", index, id))
		buf.Write(row)
		buf.WriteString("\n")
	}
	burl := fmt.Sprintf("%s/_bulk?refresh=wait_for", ix.osURL)
	breq, _ := http.NewRequestWithContext(ctx, http.MethodPost, burl, &buf)
	breq.Header.Set("Content-Type", "application/x-ndjson")
	ix.setAuth(breq)
	bresp, err := ix.client.Do(breq)
	if err != nil {
		return err
	}
	bbody, _ := io.ReadAll(bresp.Body)
	bresp.Body.Close()
	if bresp.StatusCode >= 300 {
		return fmt.Errorf("bulk: HTTP %d %s", bresp.StatusCode, strings.TrimSpace(string(bodySafe(bbody))))
	}
	var br struct {
		Errors bool `json:"errors"`
	}
	json.Unmarshal(bbody, &br)
	if br.Errors {
		return fmt.Errorf("bulk reported item errors")
	}
	return nil
}

func (ix *indexer) setAuth(req *http.Request) {
	req.SetBasicAuth(ix.osUser, ix.osPass)
}

func bodySafe(b []byte) []byte {
	if len(b) > 512 {
		return b[:512]
	}
	return b
}

func urlQueryEscape(s string) string {
	// PostgREST select lists are fine unescaped except spaces/commas we control.
	return strings.ReplaceAll(s, " ", "")
}
