package main

import (
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VideoTrafficRecord is one ad airing captured from vMix (Program or overlay).
type VideoTrafficRecord struct {
	PlayedAt        string `json:"played_at"` // "2026-10-16 06:16:28" (EAT) or RFC3339
	ChannelID       int    `json:"channel_id"`
	Monitor         string `json:"monitor"` // "Program" or "Overlay N"
	MonitorNumber   int    `json:"monitor_number"`
	TrackType       string `json:"track_type"`
	Client          string `json:"client"`
	Title           string `json:"title"`
	Artist          string `json:"artist"`
	DurationSeconds int    `json:"duration_seconds"`
	Category        string `json:"category"`
	Filename        string `json:"filename"`
	InputNumber     int    `json:"input_number"`
	InputKey        string `json:"input_key"`
	LogHash         string `json:"log_hash"`
}

type videoTrafficIngestRequest struct {
	ChannelID   int                  `json:"channel_id"`
	ChannelName string               `json:"channel_name"`
	Records     []VideoTrafficRecord `json:"records"`
}

var videoLogColumns = []string{
	"channel_id", "channel_name", "played_at", "monitor", "monitor_number",
	"track_type", "client", "title", "artist", "duration_seconds", "category",
	"filename", "input_number", "input_key", "log_hash",
}

func computeVideoTrafficHash(channelID int, playedAt time.Time, filename, title string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%d|%d|%s|%s", channelID, playedAt.Unix(), filename, title)))
	return hex.EncodeToString(sum[:])
}

// handleIngestVideoTrafficLogs receives batches from the vMix script / agent.
func (s *server) handleIngestVideoTrafficLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}

	var req videoTrafficIngestRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	if req.ChannelID == 0 {
		req.ChannelID = 401
	}
	if len(req.Records) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "inserted": 0})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inserted, err := s.insertVideoTrafficRecords(ctx, req.ChannelID, req.ChannelName, req.Records)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "insert failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"channel":  req.ChannelID,
		"received": len(req.Records),
		"inserted": inserted,
	})
}

func (s *server) insertVideoTrafficRecords(ctx context.Context, channelID int, channelName string, records []VideoTrafficRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}

	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		playedAt, perr := parseTrafficTimestamp(rec.PlayedAt)
		if perr != nil {
			continue
		}
		chID := rec.ChannelID
		if chID == 0 {
			chID = channelID
		}
		mon := strings.TrimSpace(rec.Monitor)
		if mon == "" {
			mon = "Program"
		}
		trackType := strings.TrimSpace(rec.TrackType)
		if trackType == "" {
			trackType = "Spot"
		}
		hash := strings.TrimSpace(rec.LogHash)
		if hash == "" {
			hash = computeVideoTrafficHash(chID, playedAt, rec.Filename, rec.Title)
		}
		client := s.resolveVideoClient(ctx, rec.Title, rec.Filename, rec.Client)
		title := s.resolveTrafficTitle(ctx, rec.Title, rec.Filename)

		rows = append(rows, map[string]any{
			"channel_id":       chID,
			"channel_name":     channelName,
			"played_at":        playedAt,
			"monitor":          mon,
			"monitor_number":   rec.MonitorNumber,
			"track_type":       trackType,
			"client":           client,
			"title":            title,
			"artist":           rec.Artist,
			"duration_seconds": rec.DurationSeconds,
			"category":         rec.Category,
			"filename":         rec.Filename,
			"input_number":     rec.InputNumber,
			"input_key":        rec.InputKey,
			"log_hash":         hash,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}

	db, err := s.tsdbDB(ctx)
	if err != nil {
		return 0, err
	}

	total := 0
	chunkSize := 100
	for i := 0; i < len(rows); i += chunkSize {
		end := i + chunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[i:end]
		placeholders, args := insertPlaceholders(chunk, videoLogColumns)
		stmt := fmt.Sprintf(
			"INSERT INTO public.video_spot_logs (%s) VALUES %s ON CONFLICT (played_at, log_hash) DO NOTHING",
			strings.Join(videoLogColumns, ","), placeholders)
		res, err := db.ExecContext(ctx, stmt, args...)
		if err != nil {
			return total, err
		}
		if n, rerr := res.RowsAffected(); rerr == nil {
			total += int(n)
		}
	}
	return total, nil
}

func videoTimeRange(r *http.Request) (string, string) {
	q := r.URL.Query()
	days := atoiDefault(q.Get("days"), 30)
	start := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)
	if v := q.Get("start"); v != "" {
		start = v
	}
	if v := q.Get("end"); v != "" {
		end = v
	}
	return start, end
}

func (s *server) handleGetVideoTrafficAdvertisers(w http.ResponseWriter, r *http.Request) {
	start, end := videoTimeRange(r)
	channel := atoiDefault(r.URL.Query().Get("channel"), 0)
	args := map[string]any{"p_start": start, "p_end": end, "p_channel": channel}
	s.callRadioRPC(w, r, "get_video_traffic_advertiser_summary",
		[]string{"p_start", "p_end", "p_channel"}, args)
}

func (s *server) handleGetVideoTrafficClients(w http.ResponseWriter, r *http.Request) {
	start, end := videoTimeRange(r)
	channel := atoiDefault(r.URL.Query().Get("channel"), 0)
	args := map[string]any{"p_start": start, "p_end": end, "p_channel": channel}
	s.callRadioRPC(w, r, "get_video_traffic_clients",
		[]string{"p_start", "p_end", "p_channel"}, args)
}

func (s *server) handleGetVideoTrafficAffidavit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client := q.Get("client")
	channel := atoiDefault(q.Get("channel"), 0)
	format := strings.ToLower(q.Get("format"))
	start, end := videoTimeRange(r)

	args := map[string]any{
		"p_client": client, "p_start": start, "p_end": end, "p_channel": channel,
	}
	raw := s.callRadioRPCRaw(w, r, "get_video_traffic_affidavit",
		[]string{"p_client", "p_start", "p_end", "p_channel"}, args)
	if raw == nil {
		return
	}

	if format != "csv" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	var result struct {
		Client              string `json:"client"`
		ChannelName         string `json:"channel_name"`
		TotalSpots          int    `json:"total_spots"`
		TotalAirtimeSeconds int    `json:"total_airtime_seconds"`
		Spots               []struct {
			PlayedAtLocal     string `json:"played_at_local"`
			ChannelName       string `json:"channel_name"`
			Monitor           string `json:"monitor"`
			Client            string `json:"client"`
			Title             string `json:"title"`
			DurationFormatted string `json:"duration_formatted"`
			ProgramName       string `json:"program_name"`
			Operator          string `json:"operator"`
			Filename          string `json:"filename"`
		} `json:"spots"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "csv unmarshal: " + err.Error()})
		return
	}

	cleanClient := strings.ReplaceAll(client, " ", "_")
	if cleanClient == "" {
		cleanClient = "All_Advertisers"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"TV_Affidavit_%s_%s.csv\"", cleanClient, time.Now().Format("20060102")))
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	defer cw.Flush()

	_ = cw.Write([]string{"SALT TV - PROOF OF PERFORMANCE AFFIDAVIT"})
	_ = cw.Write([]string{"Client", result.Client, "Total Spots", strconv.Itoa(result.TotalSpots),
		"Total Airtime", fmt.Sprintf("%dm %ds", result.TotalAirtimeSeconds/60, result.TotalAirtimeSeconds%60)})
	_ = cw.Write([]string{})
	_ = cw.Write([]string{"Air Date & Time (EAT)", "Channel", "Monitor", "Client", "Ad Title", "Duration", "Show / Program", "Operator", "Video File"})
	for _, sp := range result.Spots {
		_ = cw.Write([]string{
			sp.PlayedAtLocal, sp.ChannelName, sp.Monitor, sp.Client, sp.Title,
			sp.DurationFormatted, sp.ProgramName, sp.Operator, sp.Filename,
		})
	}
}

// handleUploadVideoTrafficLogFile ingests a VLog-*.csv directly (multipart POST).
func (s *server) handleUploadVideoTrafficLogFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing file form field: " + err.Error()})
		return
	}
	defer file.Close()

	channel := atoiDefault(r.URL.Query().Get("channel"), 401)
	channelName := r.URL.Query().Get("channel_name")

	records, perr := parseVideoLogCSV(file, channel)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parse failed: " + perr.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inserted, ierr := s.insertVideoTrafficRecords(ctx, channel, channelName, records)
	if ierr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "insert failed: " + ierr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "parsed": len(records), "inserted": inserted})
}

func parseVideoLogCSV(rd io.Reader, defaultChannel int) ([]VideoTrafficRecord, error) {
	reader := csv.NewReader(rd)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	var rows []VideoTrafficRecord
	defaults := []string{"played_at", "channel_id", "monitor", "client", "title",
		"duration_seconds", "filename", "input_number", "input_key", "log_hash"}

	header, err := reader.Read()
	if err == io.EOF {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}

	idx := map[string]int{}
	hasHeader := false
	for i, h := range header {
		clean := strings.TrimSpace(strings.ToLower(strings.TrimPrefix(h, "\ufeff")))
		if clean != "" {
			idx[clean] = i
		}
	}
	if _, ok := idx["played_at"]; ok {
		hasHeader = true
	} else {
		for i, name := range defaults {
			idx[name] = i
		}
	}

	get := func(row []string, name string) string {
		if i, ok := idx[name]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}

	build := func(row []string) (VideoTrafficRecord, bool) {
		playedAt := get(row, "played_at")
		title := get(row, "title")
		filename := get(row, "filename")
		if playedAt == "" || (title == "" && filename == "") {
			return VideoTrafficRecord{}, false
		}
		chID, _ := strconv.Atoi(get(row, "channel_id"))
		if chID == 0 {
			chID = defaultChannel
		}
		dur, _ := strconv.Atoi(get(row, "duration_seconds"))
		inNum, _ := strconv.Atoi(get(row, "input_number"))
		mon := get(row, "monitor")
		if mon == "" {
			mon = "Program"
		}
		return VideoTrafficRecord{
			PlayedAt:        playedAt,
			ChannelID:       chID,
			Monitor:         mon,
			MonitorNumber:   monitorNumFromString(mon),
			TrackType:       "Spot",
			Client:          get(row, "client"),
			Title:           title,
			DurationSeconds: dur,
			Filename:        filename,
			InputNumber:     inNum,
			InputKey:        get(row, "input_key"),
			LogHash:         get(row, "log_hash"),
		}, true
	}

	if !hasHeader {
		if rec, ok := build(header); ok {
			rows = append(rows, rec)
		}
	}
	for {
		row, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			continue
		}
		if rec, ok := build(row); ok {
			rows = append(rows, rec)
		}
	}
	return rows, nil
}

func monitorNumFromString(monitor string) int {
	m := strings.ToLower(strings.TrimSpace(monitor))
	if strings.HasPrefix(m, "overlay") {
		fields := strings.Fields(m)
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				return n
			}
		}
	}
	return 0
}
