package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// stationLocation is Salt FM / Kampala timezone (UTC+3)
var stationLocation = time.FixedZone("EAT", 3*3600)

// TrafficRecord represents one aired spot / track from StationPlaylist Studio
type TrafficRecord struct {
	PlayedAt        string `json:"played_at"`        // "14/10/2026 08:16:41" or RFC3339
	ScheduledAt     string `json:"scheduled_at"`     // "13/10/2026 23:00" or RFC3339
	TrackType       string `json:"track_type"`       // "Spot", "Manual", etc.
	Client          string `json:"client"`           // Advertiser / Sponsor name
	Title           string `json:"title"`            // Ad copy title
	Artist          string `json:"artist"`           // Campaign / artist
	DurationSeconds int    `json:"duration_seconds"` // Airtime in seconds
	Category        string `json:"category"`         // Category / Spot Group
	Filename        string `json:"filename"`         // Playout path e.g. F:\Adverts\...
	UserDJ          string `json:"user_dj"`          // Playout user/presenter
	Listeners       int    `json:"listeners"`
}

type trafficIngestRequest struct {
	StationID int             `json:"station_id"`
	Records   []TrafficRecord `json:"records"`
}

// clientRule maps a filename/title pattern to an advertiser name.
type clientRule struct {
	client  string
	pattern *regexp.Regexp
}

// defaultClientPatterns is the built-in fallback used when the DB rule table
// (public.traffic_client_rules) is unreachable. DB rules take precedence.
var defaultClientPatterns = []clientRule{
	{"Minute Maid", regexp.MustCompile(`(?i)MINUTE\s*MAID`)},
	{"MTN", regexp.MustCompile(`(?i)\bMTN\b`)},
	{"Airtel", regexp.MustCompile(`(?i)\bAIRTEL\b`)},
	{"Plascon", regexp.MustCompile(`(?i)PLASCON`)},
	{"Syova Seeds", regexp.MustCompile(`(?i)SYOVA`)},
	{"Advanta Seeds", regexp.MustCompile(`(?i)ADVANTA`)},
	{"Global Paints", regexp.MustCompile(`(?i)GLOBAL\s*PAINTS`)},
	{"Ebenezer Hardware", regexp.MustCompile(`(?i)EBENEZER`)},
	{"Kubera Motors", regexp.MustCompile(`(?i)KUBERA`)},
	{"URA", regexp.MustCompile(`(?i)\b(URA|EFRIS)\b`)},
	{"Levixone Concert", regexp.MustCompile(`(?i)LEVIXONE`)},
	{"Sky View", regexp.MustCompile(`(?i)SKY\s*VIEW`)},
	{"Salt Media (House Promo)", regexp.MustCompile(`(?i)SALT\s*(MEDIA|APP|LUG\s*APP|ENG\s*APP)`)},
	{"Health Tip (Sponsored)", regexp.MustCompile(`(?i)HEALTH\s*TIP`)},
	{"UPPC", regexp.MustCompile(`(?i)\bUPPC\b`)},
}

// trafficRulesTTL controls how long the DB-backed advertiser rules are cached.
const trafficRulesTTL = 5 * time.Minute

// loadClientRules returns advertiser match rules, preferring the DB table and
// falling back to last-known-good / the built-in list when unavailable.
func (s *server) loadClientRules(ctx context.Context) []clientRule {
	s.trafficRulesMu.Lock()
	defer s.trafficRulesMu.Unlock()

	if s.trafficRules != nil && time.Now().Before(s.trafficRulesExpiry) {
		return s.trafficRules
	}

	rules, err := s.fetchClientRules(ctx)
	if err != nil || len(rules) == 0 {
		if len(s.trafficRules) > 0 {
			return s.trafficRules // keep last-known-good
		}
		return defaultClientPatterns
	}

	s.trafficRules = rules
	s.trafficRulesExpiry = time.Now().Add(trafficRulesTTL)
	return rules
}

// fetchClientRules reads and compiles public.traffic_client_rules from TimescaleDB.
func (s *server) fetchClientRules(ctx context.Context) ([]clientRule, error) {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT client_name, match_pattern
		FROM public.traffic_client_rules
		ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []clientRule
	for rows.Next() {
		var name, pattern string
		if err := rows.Scan(&name, &pattern); err != nil {
			continue
		}
		name = strings.TrimSpace(name)
		pattern = strings.TrimSpace(pattern)
		if name == "" || pattern == "" {
			continue
		}
		re, cerr := regexp.Compile("(?i)" + pattern)
		if cerr != nil {
			log.Printf("[Traffic] skipping invalid client rule %q: %v", pattern, cerr)
			continue
		}
		rules = append(rules, clientRule{client: name, pattern: re})
	}
	return rules, rows.Err()
}

// titleOverride maps a title/filename pattern to a corrected creative title.
type titleOverride struct {
	title   string
	pattern *regexp.Regexp
}

// loadTitleOverrides returns per-creative title overrides, cached like the rules.
func (s *server) loadTitleOverrides(ctx context.Context) []titleOverride {
	s.titleOverridesMu.Lock()
	defer s.titleOverridesMu.Unlock()

	if s.titleOverrides != nil && time.Now().Before(s.titleOverridesExpiry) {
		return s.titleOverrides
	}

	overrides, err := s.fetchTitleOverrides(ctx)
	if err != nil {
		return s.titleOverrides // keep last-known-good (may be nil)
	}

	s.titleOverrides = overrides
	s.titleOverridesExpiry = time.Now().Add(trafficRulesTTL)
	return overrides
}

// fetchTitleOverrides reads and compiles public.traffic_title_overrides.
func (s *server) fetchTitleOverrides(ctx context.Context) ([]titleOverride, error) {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT match_pattern, title_override
		FROM public.traffic_title_overrides
		ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var overrides []titleOverride
	for rows.Next() {
		var title, pattern string
		if err := rows.Scan(&pattern, &title); err != nil {
			continue
		}
		title = strings.TrimSpace(title)
		pattern = strings.TrimSpace(pattern)
		if title == "" || pattern == "" {
			continue
		}
		re, cerr := regexp.Compile("(?i)" + pattern)
		if cerr != nil {
			log.Printf("[Traffic] skipping invalid title override %q: %v", pattern, cerr)
			continue
		}
		overrides = append(overrides, titleOverride{title: title, pattern: re})
	}
	return overrides, rows.Err()
}

// resolveTrafficTitle applies any per-creative title override, else returns the
// original StationPlaylist title.
func (s *server) resolveTrafficTitle(ctx context.Context, title, filename string) string {
	target := title + " " + filename
	for _, o := range s.loadTitleOverrides(ctx) {
		if o.pattern.MatchString(target) {
			return o.title
		}
	}
	return title
}

// nonClientPrefixes are generic words that must not be treated as an advertiser
// when parsing the studio "Client - Ad Name.mp3" naming convention.
var nonClientPrefixes = map[string]bool{
	"ad": true, "ads": true, "advert": true, "adverts": true, "advertisement": true,
	"jingle": true, "promo": true, "spot": true, "commercial": true, "sting": true,
	"announcement": true, "audio": true, "voice": true, "track": true,
}

// clientFromFilename derives the advertiser from the studio naming convention
// "Client - Ad Name.mp3" (advertiser = text before the first " - "). Returns ""
// when the prefix is absent or not a plausible name.
func clientFromFilename(filename string, rules []clientRule) string {
	if filename == "" {
		return ""
	}
	base := filename
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSpace(base)
	for _, ext := range []string{".mpeg.mp3", ".mp3.mpeg", ".mp3", ".mpeg", ".wav", ".mp4", ".aac", ".m4a", ".ogg"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = strings.TrimSpace(base[:len(base)-len(ext)])
		}
	}

	idx := strings.Index(base, " - ")
	if idx <= 0 {
		return ""
	}
	name := strings.TrimSpace(base[:idx])
	if !isPlausibleClient(name) {
		return ""
	}
	// Prefer the canonical spelling when this prefix is a known advertiser.
	for _, r := range rules {
		if strings.EqualFold(r.client, name) {
			return r.client
		}
	}
	return name
}

func isPlausibleClient(name string) bool {
	if len(name) < 2 || len(name) > 40 {
		return false
	}
	if nonClientPrefixes[strings.ToLower(name)] {
		return false
	}
	for _, r := range name {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

var adFilenameRe = regexp.MustCompile(`^(.+?\S)-(\S.*)$`)

// clientFromAdFilename parses the "Client-AdName.ext" convention: the
// advertiser is the text before the first hyphen that has no surrounding
// spaces (so prose titles like "Show - Part 2" are ignored).
func clientFromAdFilename(filename, title string) string {
	name := strings.TrimSpace(filename)
	if name == "" {
		name = strings.TrimSpace(title)
	}
	if name == "" {
		return ""
	}
	base := name
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSpace(base)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	m := adFilenameRe.FindStringSubmatch(base)
	if len(m) != 3 {
		return ""
	}
	candidate := strings.ReplaceAll(strings.TrimSpace(m[1]), "_", " ")
	candidate = strings.Join(strings.Fields(candidate), " ")
	if !isPlausibleClient(candidate) {
		return ""
	}
	return candidate
}

// resolveTrafficClient infers the advertiser when the Client column is empty in
// manual logs. Precedence: explicit client → DB-driven rules →
// filename "Client - Ad Name" prefix → "Other Advert" / "Unassigned".
func (s *server) resolveTrafficClient(ctx context.Context, title, filename, explicit string) string {
	explicit = strings.TrimSpace(explicit)
	// A caller-side fallback marker is not a real client. Re-resolve it against
	// the DB rules so rows logged before a rule existed can still be attributed.
	if !isUnresolvedClient(explicit) {
		return explicit
	}

	rules := s.loadClientRules(ctx)
	target := title + " " + filename
	for _, p := range rules {
		if p.pattern.MatchString(target) {
			return p.client
		}
	}
	if c := clientFromFilename(filename, rules); c != "" {
		return c
	}
	if explicit != "" {
		return explicit
	}
	if strings.Contains(strings.ToLower(filename), "advert") || strings.Contains(strings.ToLower(title), "advert") {
		return "Other Advert"
	}
	return "Unassigned"
}

func isUnresolvedClient(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" ||
		strings.EqualFold(name, "Other Advert") ||
		strings.EqualFold(name, "Unassigned") ||
		strings.EqualFold(name, "Unknown")
}

// resolveVideoClient treats the DB rules and the Client-AdName filename
// convention as authoritative for TV; the caller's value is a last resort.
func (s *server) resolveVideoClient(ctx context.Context, title, filename, explicit string) string {
	rules := s.loadClientRules(ctx)
	target := title + " " + filename
	for _, p := range rules {
		if p.pattern.MatchString(target) {
			return p.client
		}
	}
	if c := clientFromFilename(filename, rules); c != "" {
		return c
	}
	if c := clientFromAdFilename(filename, title); c != "" {
		return c
	}
	explicit = strings.TrimSpace(explicit)
	if !isUnresolvedClient(explicit) {
		return explicit
	}
	if explicit != "" {
		return explicit
	}
	if strings.Contains(strings.ToLower(filename), "advert") || strings.Contains(strings.ToLower(title), "advert") {
		return "Other Advert"
	}
	return "Unassigned"
}

// parseTrafficTimestamp parses StationPlaylist dates (DD/MM/YYYY HH:MM:SS) or ISO
func parseTrafficTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}

	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}

	// Try StationPlaylist formats: "14/10/2026 08:16:41" or "14/10/2026 08:16"
	layouts := []string{
		"02/01/2006 15:04:05",
		"02/01/2006 15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"02-01-2006 15:04:05",
		"02-01-2006 15:04",
	}

	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, stationLocation); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unknown date format: %q", raw)
}

// parseDurationMMSS converts "00:30" or "04:29" or raw seconds into integer seconds
func parseDurationMMSS(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if sec, err := strconv.Atoi(raw); err == nil {
		return sec
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 2 {
		m, _ := strconv.Atoi(parts[0])
		s, _ := strconv.Atoi(parts[1])
		return m*60 + s
	} else if len(parts) == 3 {
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		s, _ := strconv.Atoi(parts[2])
		return h*3600 + m*60 + s
	}
	return 0
}

func computeTrafficHash(stationID int, playedAt time.Time, filename, title string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%d|%d|%s|%s", stationID, playedAt.Unix(), filename, title)))
	return hex.EncodeToString(sum[:])
}

// handleIngestTrafficLogs receives batches from the StationPlaylist agent / script
func (s *server) handleIngestTrafficLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}

	var req trafficIngestRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}

	if req.StationID == 0 {
		req.StationID = 307 // Default Salt FM
	}

	if len(req.Records) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "inserted": 0})
		return
	}

	if s.outbox != nil {
		s.enqueueRadioIngest(w, req.StationID, req.Records, 0)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inserted, err := s.insertTrafficRecords(ctx, req.StationID, req.Records)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "insert failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"station":  req.StationID,
		"received": len(req.Records),
		"inserted": inserted,
	})
}

// enqueueRadioJob durably buffers a radio traffic batch.
func (s *server) enqueueRadioJob(stationID int, records []TrafficRecord) (bool, error) {
	receivedAt := time.Now().UTC()
	payload := radioIngestJob{StationID: stationID, Records: records, ReceivedAt: receivedAt}
	return s.outbox.Enqueue(outboxKindRadioIngest, radioTrafficDedupeKey(payload), payload, receivedAt)
}

// enqueueRadioIngest writes the HTTP response used by the radio agent and CSV
// upload. HTTP 200 keeps the existing agents' success checks unchanged.
func (s *server) enqueueRadioIngest(w http.ResponseWriter, stationID int, records []TrafficRecord, parsed int) {
	duplicate, err := s.enqueueRadioJob(stationID, records)
	if err != nil {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "log buffer unavailable: " + err.Error(),
			"buffered": false, "received": len(records),
		})
		return
	}
	response := map[string]any{
		"success": true, "station": stationID, "received": len(records),
		"inserted": 0, "queued": len(records), "buffered": true, "duplicate": duplicate,
	}
	if parsed > 0 {
		response["parsed"] = parsed
	}
	writeJSON(w, http.StatusOK, response)
}

func radioTrafficDedupeKey(payload radioIngestJob) string {
	canonical := struct {
		StationID int             `json:"station_id"`
		Records   []TrafficRecord `json:"records"`
	}{StationID: payload.StationID, Records: payload.Records}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// insertTrafficRecords performs fast bulk-inserts using multi-row batching
func (s *server) insertTrafficRecords(ctx context.Context, stationID int, records []TrafficRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}

	cols := []string{
		"station_id", "played_at", "scheduled_at", "track_type", "client", "title", "artist",
		"duration_seconds", "category", "filename", "user_dj", "listeners", "variance_seconds", "log_hash",
	}

	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		playedAt, perr := parseTrafficTimestamp(rec.PlayedAt)
		if perr != nil {
			continue
		}

		var schedAt any
		var variance any
		if rec.ScheduledAt != "" {
			if st, serr := parseTrafficTimestamp(rec.ScheduledAt); serr == nil {
				schedAt = st
				diff := int(playedAt.Sub(st).Seconds())
				variance = diff
			}
		}

		client := s.resolveTrafficClient(ctx, rec.Title, rec.Filename, rec.Client)
		title := s.resolveTrafficTitle(ctx, rec.Title, rec.Filename)
		trackType := rec.TrackType
		if trackType == "" || trackType == "0" {
			trackType = "Spot"
		}
		hash := computeTrafficHash(stationID, playedAt, rec.Filename, rec.Title)

		row := map[string]any{
			"station_id":       stationID,
			"played_at":        playedAt,
			"scheduled_at":     schedAt,
			"track_type":       trackType,
			"client":           client,
			"title":            title,
			"artist":           rec.Artist,
			"duration_seconds": rec.DurationSeconds,
			"category":         rec.Category,
			"filename":         rec.Filename,
			"user_dj":          rec.UserDJ,
			"listeners":        rec.Listeners,
			"variance_seconds": variance,
			"log_hash":         hash,
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return 0, nil
	}

	db, err := s.tsdbDB(ctx)
	if err != nil {
		return 0, err
	}

	totalInserted := 0
	chunkSize := 100
	for i := 0; i < len(rows); i += chunkSize {
		end := i + chunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[i:end]

		placeholders, args := insertPlaceholders(chunk, cols)
		stmt := fmt.Sprintf("INSERT INTO public.radio_spot_logs (%s) VALUES %s ON CONFLICT (played_at, log_hash) DO NOTHING",
			strings.Join(cols, ","), placeholders)

		res, err := db.ExecContext(ctx, stmt, args...)
		if err != nil {
			return totalInserted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			totalInserted += int(n)
		}
	}

	return totalInserted, nil
}

// handleStationPlaylistWebhook handles real-time HTTP GET/POST from StationPlaylist Studio
// Configured in: Studio -> Options -> Now Playing -> Stream Metadata -> HTTP Request
func (s *server) handleStationPlaylistWebhook(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	artist := q.Get("artist")
	title := q.Get("title")
	cat := q.Get("cat")
	durStr := q.Get("dur")
	file := q.Get("file")
	dateStr := q.Get("date")
	timeStr := q.Get("time")
	clientParam := q.Get("client")
	stationStr := q.Get("station")

	stationID := 307
	if n, err := strconv.Atoi(stationStr); err == nil && n > 0 {
		stationID = n
	}

	// Filter: only log if it's an advert or explicitly marked
	isAdvert := strings.Contains(strings.ToLower(file), "advert") ||
		strings.Contains(strings.ToLower(title), "advert") ||
		strings.Contains(strings.ToLower(cat), "spot") ||
		strings.Contains(strings.ToLower(cat), "commercial")

	if !isAdvert && q.Get("all") != "true" {
		// Acknowledge without inserting music if not requested
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("IGNORED_NON_ADVERT"))
		return
	}

	playedAt := time.Now().In(stationLocation)
	if dateStr != "" && timeStr != "" {
		if parsed, err := parseTrafficTimestamp(dateStr + " " + timeStr); err == nil {
			playedAt = parsed
		}
	}

	dur := parseDurationMMSS(durStr)

	if s.outbox != nil {
		rec := TrafficRecord{
			PlayedAt:        playedAt.Format(time.RFC3339),
			TrackType:       "Spot",
			Client:          clientParam,
			Title:           title,
			Artist:          artist,
			DurationSeconds: dur,
			Category:        cat,
			Filename:        file,
		}
		if _, err := s.enqueueRadioJob(stationID, []TrafficRecord{rec}); err != nil {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}

	hash := computeTrafficHash(stationID, playedAt, file, title)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	client := s.resolveTrafficClient(ctx, title, file, clientParam)
	storedTitle := s.resolveTrafficTitle(ctx, title, file)

	db, err := s.tsdbDB(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}

	query := `
		INSERT INTO public.radio_spot_logs (
			station_id, played_at, track_type, client, title, artist,
			duration_seconds, category, filename, log_hash
		) VALUES ($1, $2, 'Spot', $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (played_at, log_hash) DO NOTHING
	`
	_, _ = db.ExecContext(ctx, query, stationID, playedAt, client, storedTitle, artist, dur, cat, file, hash)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleGetTrafficAdvertisers returns the summary list of advertisers & spot counts
func (s *server) handleGetTrafficAdvertisers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := atoiDefault(q.Get("days"), 30)
	station := atoiDefault(q.Get("station"), 307)

	start := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)
	if sVal := q.Get("start"); sVal != "" {
		start = sVal
	}
	if eVal := q.Get("end"); eVal != "" {
		end = eVal
	}

	args := map[string]any{
		"p_start":   start,
		"p_end":     end,
		"p_station": station,
	}
	s.callRadioRPC(w, r, "get_traffic_advertiser_summary", []string{"p_start", "p_end", "p_station"}, args)
}

// handleGetTrafficAffidavit returns itemized proof of performance, supports CSV export
func (s *server) handleGetTrafficAffidavit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client := q.Get("client")
	days := atoiDefault(q.Get("days"), 30)
	station := atoiDefault(q.Get("station"), 307)
	format := strings.ToLower(q.Get("format"))

	start := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)
	if sVal := q.Get("start"); sVal != "" {
		start = sVal
	}
	if eVal := q.Get("end"); eVal != "" {
		end = eVal
	}

	args := map[string]any{
		"p_client":  client,
		"p_start":   start,
		"p_end":     end,
		"p_station": station,
	}

	raw := s.callRadioRPCRaw(w, r, "get_traffic_affidavit", []string{"p_client", "p_start", "p_end", "p_station"}, args)
	if raw == nil {
		return
	}

	if format != "csv" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	// Format as CSV for Traffic Officer / Billing
	var result struct {
		Client              string `json:"client"`
		TotalSpots          int    `json:"total_spots"`
		TotalAirtimeSeconds int    `json:"total_airtime_seconds"`
		Spots               []struct {
			PlayedAtLocal     string `json:"played_at_local"`
			Client            string `json:"client"`
			Title             string `json:"title"`
			DurationFormatted string `json:"duration_formatted"`
			ProgramName       string `json:"program_name"`
			UserDJ            string `json:"user_dj"`
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"Affidavit_%s_%s.csv\"", cleanClient, time.Now().Format("20060102")))
	w.WriteHeader(http.StatusOK)

	csvW := csv.NewWriter(w)
	defer csvW.Flush()

	_ = csvW.Write([]string{"SALT FM — PROOF OF PERFORMANCE AFFIDAVIT"})
	_ = csvW.Write([]string{"Client", result.Client, "Total Spots", strconv.Itoa(result.TotalSpots), "Total Airtime", fmt.Sprintf("%dm %ds", result.TotalAirtimeSeconds/60, result.TotalAirtimeSeconds%60)})
	_ = csvW.Write([]string{}) // blank line
	_ = csvW.Write([]string{"Air Date & Time (EAT)", "Client", "Ad Title", "Duration", "Show / Program", "DJ / Playout", "Audio File"})

	for _, sp := range result.Spots {
		_ = csvW.Write([]string{
			sp.PlayedAtLocal,
			sp.Client,
			sp.Title,
			sp.DurationFormatted,
			sp.ProgramName,
			sp.UserDJ,
			sp.Filename,
		})
	}
}

// handleUploadTrafficLogFile parses a StationPlaylist SLog-*.csv file directly via multipart POST
func (s *server) handleUploadTrafficLogFile(w http.ResponseWriter, r *http.Request) {
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

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1 // Allow variable columns
	reader.LazyQuotes = true

	var records []TrafficRecord
	for {
		line, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil || len(line) < 9 {
			continue
		}

		filename := line[8]
		// Filter: only advert files
		if !strings.Contains(strings.ToLower(filename), "advert") {
			continue
		}

		dateStr := strings.TrimPrefix(line[0], "\ufeff") // strip BOM
		timeStr := line[1]
		userDJ := line[2]
		trackType := line[3]
		artist := line[4]
		title := line[5]
		dur := parseDurationMMSS(line[6])
		cat := line[7]
		explicitClient := ""
		if len(line) > 9 {
			explicitClient = line[9]
		}
		sched := ""
		if len(line) > 17 {
			sched = line[17]
		}

		records = append(records, TrafficRecord{
			PlayedAt:        dateStr + " " + timeStr,
			ScheduledAt:     sched,
			TrackType:       trackType,
			Client:          explicitClient,
			Title:           title,
			Artist:          artist,
			DurationSeconds: dur,
			Category:        cat,
			Filename:        filename,
			UserDJ:          userDJ,
		})
	}

	if s.outbox != nil {
		s.enqueueRadioIngest(w, 307, records, len(records))
		return
	}

	// Reuse ingestion logic
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inserted, err := s.insertTrafficRecords(ctx, 307, records)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "insert failed: " + err.Error()})
		return
	}

	log.Printf("[Traffic] Uploaded CSV: parsed %d advert records, inserted %d new", len(records), inserted)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"parsed":   len(records),
		"inserted": inserted,
	})
}
