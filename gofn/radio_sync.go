package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Radio sync jobs — pull from the external AzuraCast API straight into
// TimescaleDB (no BigQuery in between). Mirrors functions/src/azuracast.js
// syncStationHistory / syncAzuraCastCharts / snapshotAzuraCastListeners /
// snapshotNowPlayingTotals. The readers already read TSDB, so these are the
// only writers needed.

const (
	azuraCastBase = "https://a7.asurahosting.com"
	saltFMStation = 307
)

func (s *server) azuracastURL(path string) string {
	key := getenv("AZURACAST_API_KEY", "")
	return fmt.Sprintf("%s%s?api_key=%s", azuraCastBase, path, url.QueryEscape(key))
}

func (s *server) azuracastGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.azuracastURL(path), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("azuracast %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// isTimestampCol reports whether a column is timestamptz (needs ISO→time).
func isTimestampCol(k string) bool {
	switch k {
	case "played_at", "snapshot_at", "connected_on", "connected_until", "synced_at", "date":
		return true
	}
	return false
}

// upsertRows inserts rows, updating conflicting rows keyed by conflictCols
// (e.g. radio_daily (station_id, date)). Timestamp cols parsed from ISO.
func (s *server) upsertRows(ctx context.Context, table string, rows []map[string]any, conflictCols []string) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		for k, v := range row {
			if isTimestampCol(k) {
				if str, ok := v.(string); ok && str != "" {
					if t, err := time.Parse(time.RFC3339, str); err == nil {
						row[k] = t
					}
				}
			}
		}
	}
	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	placeholders, args := insertPlaceholders(rows, cols)

	var sets []string
	for _, c := range cols {
		isConflict := false
		for _, cc := range conflictCols {
			if c == cc {
				isConflict = true
				break
			}
		}
		if !isConflict {
			sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
		}
	}
	stmt := fmt.Sprintf("INSERT INTO public.%s (%s) VALUES %s ON CONFLICT (%s) DO UPDATE SET %s",
		table, strings.Join(cols, ","), placeholders, strings.Join(conflictCols, ","), strings.Join(sets, ","))
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, stmt, args...)
	return err
}

// insertRows bulk-inserts rows into a TSDB hypertable (column map order = first
// row keys). Timestamp-ish columns parsed from ISO strings.
func (s *server) insertRows(ctx context.Context, table string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		for k, v := range row {
			if isTimestampCol(k) {
				if str, ok := v.(string); ok && str != "" {
					if t, err := time.Parse(time.RFC3339, str); err == nil {
						row[k] = t
					}
				}
			}
		}
	}
	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	placeholders, args := insertPlaceholders(rows, cols)
	stmt := "INSERT INTO public." + table + " (" + strings.Join(cols, ",") + ") VALUES " + placeholders
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, stmt, args...)
	return err
}

// ──────────────── HISTORY SYNC ────────────────

func (s *server) syncRadioHistory(ctx context.Context, stationID int) (int, error) {
	var history []struct {
		ShID     int `json:"sh_id"`
		PlayedAt int `json:"played_at"`
		Duration int `json:"duration"`
		Playlist string `json:"playlist"`
		Streamer string `json:"streamer"`
		IsRequest bool `json:"is_request"`
		Song     struct {
			ID     string `json:"id"`
			Artist string `json:"artist"`
			Title  string `json:"title"`
			Text   string `json:"text"`
			Album  string `json:"album"`
			Genre  string `json:"genre"`
		} `json:"song"`
		ListenersStart int  `json:"listeners_start"`
		ListenersEnd   int  `json:"listeners_end"`
		DeltaTotal     int  `json:"delta_total"`
		IsVisible      *bool `json:"is_visible"`
	}
	if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/history", stationID), &history); err != nil {
		return 0, err
	}
	if len(history) == 0 {
		return 0, nil
	}

	// Dedupe against existing sh_ids.
	shIDs := make([]int, 0, len(history))
	for _, h := range history {
		shIDs = append(shIDs, h.ShID)
	}
	existing := s.existingShIDs(ctx, shIDs)

	now := time.Now().UTC().Format(time.RFC3339)
	rows := make([]map[string]any, 0, len(history))
	for _, h := range history {
		if existing[h.ShID] {
			continue
		}
		visible := true
		if h.IsVisible != nil {
			visible = *h.IsVisible
		}
		rows = append(rows, map[string]any{
			"sh_id":            h.ShID,
			"station_id":       stationID,
			"station_name":     "SALT FM",
			"played_at":        time.Unix(int64(h.PlayedAt), 0).UTC().Format(time.RFC3339),
			"duration_seconds": h.Duration,
			"playlist":         h.Playlist,
			"streamer":         h.Streamer,
			"is_request":       h.IsRequest,
			"is_visible":       visible,
			"delta_total":      h.DeltaTotal,
			"listeners_start":  h.ListenersStart,
			"listeners_end":    h.ListenersEnd,
			"song_id":          h.Song.ID,
			"song_artist":      h.Song.Artist,
			"song_title":       h.Song.Title,
			"song_text":        h.Song.Text,
			"song_album":       h.Song.Album,
			"song_genre":       h.Song.Genre,
			"synced_at":        now,
		})
	}
	if err := s.insertRows(ctx, "radio_history", rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func (s *server) existingShIDs(ctx context.Context, shIDs []int) map[int]bool {
	out := map[int]bool{}
	if len(shIDs) == 0 {
		return out
	}
	args := make([]any, len(shIDs))
	ph := make([]string, len(shIDs))
	for i, id := range shIDs {
		args[i] = id
		ph[i] = "$" + strconv.Itoa(i+1)
	}
	rows, err := s.tsdbDB(ctx)
	if err != nil {
		return out
	}
	rs, err := rows.QueryContext(ctx,
		"SELECT sh_id FROM public.radio_history WHERE sh_id IN ("+strings.Join(ph, ",")+")", args...)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var id int
		if rs.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// ──────────────── NOWPLAYING SNAPSHOT ────────────────

func (s *server) snapshotRadioNowPlaying(ctx context.Context, stationID int) (int, error) {
	var np struct {
		Listeners struct {
			Total   int `json:"total"`
			Unique  int `json:"unique"`
			Current int `json:"current"`
		} `json:"listeners"`
		Station struct {
			Mounts []struct {
				Listeners struct {
					Current int `json:"current"`
				} `json:"listeners"`
			} `json:"mounts"`
			HlsListeners int `json:"hls_listeners"`
		} `json:"station"`
		NowPlaying struct {
			Song struct {
				Title  string `json:"title"`
				Artist string `json:"artist"`
				Text   string `json:"text"`
			} `json:"song"`
		} `json:"now_playing"`
		Live struct {
			IsLive       bool   `json:"is_live"`
			StreamerName string `json:"streamer_name"`
		} `json:"live"`
	}
	if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/nowplaying", stationID), &np); err != nil {
		return 0, err
	}

	mount := 0
	if len(np.Station.Mounts) > 0 {
		mount = np.Station.Mounts[0].Listeners.Current
	}
	fromMounts := mount + np.Station.HlsListeners
	total := np.Listeners.Total
	if total == 0 {
		total = fromMounts
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows := []map[string]any{{
		"snapshot_at":       now,
		"station_id":        stationID,
		"listeners_total":   total,
		"listeners_unique":  np.Listeners.Unique,
		"listeners_current": np.Listeners.Current,
		"hls_listeners":     np.Station.HlsListeners,
		"mount_listeners":   mount,
		"song_title":        np.NowPlaying.Song.Title,
		"song_artist":       np.NowPlaying.Song.Artist,
		"is_live":           np.Live.IsLive,
		"streamer_name":     np.Live.StreamerName,
	}}
	if err := s.insertRows(ctx, "radio_nowplaying", rows); err != nil {
		return 0, err
	}
	return 1, nil
}

// ──────────────── LISTENER SNAPSHOT ────────────────

func (s *server) snapshotRadioListeners(ctx context.Context, stationID int) (int, error) {
	var listeners []struct {
		IP          string `json:"ip"`
		Hash        string `json:"hash"`
		MountName   string `json:"mount_name"`
		ConnectedOn int64  `json:"connected_on"`
		ConnectedUntil int64 `json:"connected_until"`
		ConnectedTime  int    `json:"connected_time"`
		Device     struct {
			IsBrowser *bool  `json:"is_browser"`
			IsMobile  *bool  `json:"is_mobile"`
			IsBot     *bool  `json:"is_bot"`
			Client    string `json:"client"`
			BrowserFamily string `json:"browser_family"`
			OsFamily string `json:"os_family"`
		} `json:"device"`
	}
	if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/listeners", stationID), &listeners); err != nil {
		return 0, err
	}
	if len(listeners) == 0 {
		return 0, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rows := make([]map[string]any, 0, len(listeners))
	for _, l := range listeners {
		ip := parseClientIP(l.IP)
		rows = append(rows, map[string]any{
			"snapshot_at":    now,
			"client_ip":      ip,
			"ip_hash":        l.Hash,
			"mount_name":     l.MountName,
			"is_hls":         strings.HasPrefix(l.MountName, "HLS"),
			"connected_time": l.ConnectedTime,
			"is_browser":     boolPtr(l.Device.IsBrowser),
			"is_mobile":      boolPtr(l.Device.IsMobile),
			"is_bot":         boolPtr(l.Device.IsBot),
			"client":         l.Device.Client,
			"browser_family": l.Device.BrowserFamily,
			"os_family":      l.Device.OsFamily,
			"synced_at":      now,
		})
		if l.ConnectedOn != 0 {
			rows[len(rows)-1]["connected_on"] = time.Unix(l.ConnectedOn, 0).UTC().Format(time.RFC3339)
		}
		if l.ConnectedUntil != 0 {
			rows[len(rows)-1]["connected_until"] = time.Unix(l.ConnectedUntil, 0).UTC().Format(time.RFC3339)
		}
	}
	if err := s.insertRows(ctx, "radio_listeners", rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func parseClientIP(raw string) string {
	if i := strings.IndexByte(raw, ','); i >= 0 {
		return strings.TrimSpace(raw[:i])
	}
	return strings.TrimSpace(raw)
}

func boolPtr(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// ──────────────── CHARTS / REPORTS SYNC ────────────────

func (s *server) syncRadioCharts(ctx context.Context, stationID int) (map[string]int, error) {
	counts := map[string]int{}

	var charts struct {
		Daily struct {
			Metrics []struct {
				Data []struct {
					X json.RawMessage `json:"x"`
					Y float64         `json:"y"`
				} `json:"data"`
			} `json:"metrics"`
		} `json:"daily"`
		Hourly map[string]struct {
			Metrics []struct {
				Data []float64 `json:"data"`
			} `json:"metrics"`
		} `json:"hourly"`
	}
	if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/reports/overview/charts", stationID), &charts); err != nil {
		return counts, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	// daily — x is ms-epoch number (or ISO string in some envs); upsert on
	// (station_id, date) so re-syncs update rather than duplicate.
	if len(charts.Daily.Metrics) > 0 {
		rows := make([]map[string]any, 0, len(charts.Daily.Metrics[0].Data))
		for _, d := range charts.Daily.Metrics[0].Data {
			date := parseChartDate(d.X)
			rows = append(rows, map[string]any{"date": date, "station_id": stationID, "listeners": int(d.Y), "synced_at": now})
		}
		if err := s.upsertRows(ctx, "radio_daily", rows, []string{"station_id", "date"}); err != nil {
			return counts, err
		}
		counts["daily"] = len(rows)
	}
	// hourly
	var hourlyRows []map[string]any
	for day := 0; day < 7; day++ {
		dayKey := fmt.Sprintf("day%d", day)
		dd, ok := charts.Hourly[dayKey]
		if !ok || len(dd.Metrics) == 0 {
			continue
		}
		for hour, val := range dd.Metrics[0].Data {
			hourlyRows = append(hourlyRows, map[string]any{
				"day_of_week": day + 1, "hour": hour, "station_id": stationID,
				"listeners": int(val), "synced_at": now,
			})
		}
	}
	if len(hourlyRows) > 0 {
		if err := s.insertRows(ctx, "radio_hourly", hourlyRows); err != nil {
			return counts, err
		}
		counts["hourly"] = len(hourlyRows)
	}

	// best/worst
	var bw struct {
		MostPlayed []struct {
			Song struct {
				Text string `json:"text"`
			} `json:"song"`
			NumPlays int `json:"num_plays"`
		} `json:"mostPlayed"`
	}
	if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/reports/overview/best-and-worst", stationID), &bw); err == nil && len(bw.MostPlayed) > 0 {
		rows := make([]map[string]any, 0, len(bw.MostPlayed))
		for _, m := range bw.MostPlayed {
			rows = append(rows, map[string]any{
				"type": "most_played", "song_text": m.Song.Text, "num_plays": m.NumPlays,
				"station_id": stationID, "synced_at": now,
			})
		}
		if err := s.insertRows(ctx, "radio_best_worst", rows); err != nil {
			return counts, err
		}
		counts["best_worst"] = len(rows)
	}

	// overview reports (country/browser/client/stream/listening_time)
	overview := map[string]struct{ path, idField string }{
		"radio_country":       {"by-country", "country_code"},
		"radio_browser":       {"by-browser", "browser"},
		"radio_client":        {"by-client", "client_raw"},
		"radio_stream":        {"by-stream", "stream_id"},
		"radio_listening_time": {"by-listening-time", "label"},
	}
	for table, cfg := range overview {
		var resp struct {
			All []map[string]any `json:"all"`
		}
		if err := s.azuracastGet(ctx, fmt.Sprintf("/api/station/%d/reports/overview/%s", stationID, cfg.path), &resp); err != nil {
			continue
		}
		rows := make([]map[string]any, 0, len(resp.All))
		for _, item := range resp.All {
			row := map[string]any{"synced_at": now, "station_id": stationID}
			if table == "radio_listening_time" {
				row["label"] = str(item["label"])
				row["value"] = item["value"]
			} else {
				row["label"] = str(item[cfg.idField])
				row["value"] = firstStr(item["country"], item["client"], item["stream"])
				row["listeners"] = intVal(item["listeners"])
				row["connected_seconds"] = intVal(item["connected_seconds"])
			}
			rows = append(rows, row)
		}
		if len(rows) > 0 {
			if err := s.insertRows(ctx, table, rows); err != nil {
				log.Printf("insert %s: %v", table, err)
			} else {
				counts[table] = len(rows)
			}
		}
	}
	return counts, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstStr(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// parseChartDate converts a charts `x` value (ms-epoch number or ISO string)
// to a YYYY-MM-DD date.
func parseChartDate(raw json.RawMessage) string {
	var ms int64
	if err := json.Unmarshal(raw, &ms); err == nil {
		return time.UnixMilli(ms).UTC().Format("2006-01-02")
	}
	var iso string
	if err := json.Unmarshal(raw, &iso); err == nil {
		if t, err := time.Parse(time.RFC3339, iso); err == nil {
			return t.UTC().Format("2006-01-02")
		}
	}
	return time.Now().UTC().Format("2006-01-02")
}

func intVal(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// ──────────────── HTTP HANDLERS (manual trigger + scheduled) ────────────────

func (s *server) handleSyncRadioHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	n, err := s.syncRadioHistory(ctx, saltFMStation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "count": n})
}

func (s *server) handleSyncRadioReports(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	counts, err := s.syncRadioCharts(ctx, saltFMStation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "counts": counts})
}

func (s *server) handleSnapshotRadioListeners(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	n, err := s.snapshotRadioListeners(ctx, saltFMStation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "count": n})
}

func (s *server) handleSnapshotRadioNowPlaying(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	n, err := s.snapshotRadioNowPlaying(ctx, saltFMStation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "count": n})
}
