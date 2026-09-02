package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type server struct {
	restURL    string
	serviceKey string
	anonKey    string
	client     *http.Client

	// Optional OpenSearch-backed search (set via OPENSEARCH_URL).
	osURL      string
	osUser     string
	osPass     string
	osInsecure bool
	osClient   *http.Client

	tsdb *tsdbConn

	adsMu    sync.Mutex
	adsCache map[string]adsCacheEntry

	rpcMu    sync.Mutex
	rpcCache map[string]rpcCacheEntry

	epgMu    sync.Mutex
	epgCache map[string]epgCacheEntry

	odMu    sync.Mutex
	odCache *ondemandCacheEntry

	odThumbMu      sync.Mutex
	odThumbCache   map[string]string
	odThumbExpires time.Time

	// Serializes broadcast pushes (one at a time, TryLock for "in progress").
	broadcastMu sync.Mutex
}

type catalogParams struct {
	Q      string `json:"q"`
	Type   string `json:"type"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

func main() {
	loadDotEnv()
	tsdb, err := newTSDB()
	if err != nil {
		log.Printf("timescale disabled: %v", err)
	}
	s := &server{
		restURL:      strings.TrimSuffix(getenv("PG_REST_URL", "http://api-gw:8000/rest/v1"), "/"),
		serviceKey:   os.Getenv("SERVICE_ROLE_KEY"),
		anonKey:      os.Getenv("ANON_KEY"),
		client:       &http.Client{Timeout: 10 * time.Second},
		osURL:        strings.TrimSuffix(os.Getenv("OPENSEARCH_URL"), "/"),
		osUser:       getenv("OPENSEARCH_USER", "admin"),
		osPass:       os.Getenv("OPENSEARCH_PASSWORD"),
		osInsecure:   os.Getenv("OPENSEARCH_INSECURE") == "1",
		tsdb:         tsdb,
		adsCache:     map[string]adsCacheEntry{},
		rpcCache:     map[string]rpcCacheEntry{},
		epgCache:     map[string]epgCacheEntry{},
		odCache:      nil,
		odThumbCache: map[string]string{},
	}
	if s.osURL != "" {
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: s.osInsecure}}
		s.osClient = &http.Client{Transport: tr, Timeout: 5 * time.Second}
		log.Printf("opensearch search enabled: %s", s.osURL)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.dispatch)

	s.startScheduler()
	s.startChatScheduler()
	s.startNotificationStaleCleanup()

	port := getenv("PORT", "8080")
	log.Printf("salt-gofn listening on :%s rest=%s tsdb=%v", port, s.restURL, tsdb != nil)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) dispatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "apikey, Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	name := fnName(r.URL.Path)
	if name == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "function not found"})
		return
	}
	if name != "health" && name != "getApiConfig" && !s.publicFn(name) && !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid api key"})
		return
	}
	switch name {
	case "health":
		s.handleHealth(w, r)
	case "getApiConfig":
		s.handleGetApiConfig(w, r)
	case "catalog":
		s.handleCatalog(w, r)
	case "searchUsers":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleSearchUsers(w, r)
	case "searchArticles":
		s.handleSearchArticles(w, r)
	case "searchOnDemand":
		s.handleSearchOnDemand(w, r)
	case "searchBible":
		s.handleSearchBible(w, r)
	case "searchEpg":
		s.handleSearchEpg(w, r)
	case "getNewsArticles":
		s.handleGetNewsArticles(w, r)
	case "getNewsArticle":
		s.handleGetNewsArticle(w, r)
	case "createJoomlaArticle", "updateJoomlaArticle", "deleteJoomlaArticle":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		switch name {
		case "createJoomlaArticle":
			s.handleCreateJoomlaArticle(w, r)
		case "updateJoomlaArticle":
			s.handleUpdateJoomlaArticle(w, r)
		case "deleteJoomlaArticle":
			s.handleDeleteJoomlaArticle(w, r)
		}
	case "getJoomlaReference":
		s.handleGetJoomlaReference(w, r)
	case "uploadNewsImage":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleUploadNewsImage(w, r)
	case "listNewsImages":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleListNewsImages(w, r)
	case "createNewsFolder":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleCreateNewsFolder(w, r)
	case "deleteNewsImage":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleDeleteNewsImage(w, r)
	case "createUser", "updateUserRole", "deleteUser", "getUsersPaginated":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		switch name {
		case "createUser":
			s.handleCreateUser(w, r)
		case "updateUserRole":
			s.handleUpdateUserRole(w, r)
		case "deleteUser":
			s.handleDeleteUser(w, r)
		case "getUsersPaginated":
			s.handleGetUsersPaginated(w, r)
		}
	case "getAdMobile":
		s.handleGetAdMobile(w, r)
	case "batchTrackAdEvents":
		s.handleBatchTrackAdEvents(w, r)
	case "getAdAnalytics":
		s.handleGetAdAnalytics(w, r)
	case "refreshAdCache", "scheduleAdCacheRefresh":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleRefreshAdCache(w, r)
	case "batchTrackContentViews":
		s.trackView(w, r)
	case "batchTrackContentImpressions":
		s.trackImpression(w, r)
	case "batchTrackWatchProgress":
		s.trackWatchProgress(w, r)
	case "batchTrackContentSessions":
		s.trackContentSession(w, r)
	case "processChatMessage":
		s.handleProcessChatMessage(w, r)
	case "getAnalyticsMetrics":
		s.handleGetAnalyticsMetrics(w, r)
	case "getFirebaseAnalytics":
		s.handleGetFirebaseAnalytics(w, r)
	case "getGa4Analytics":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleGetGa4Analytics(w, r)
	case "getRadioHistory":
		s.handleGetRadioHistory(w, r)
	case "getRadioReports":
		s.handleGetRadioReports(w, r)
	case "getRadioCountryDetails":
		s.handleGetRadioCountryDetails(w, r)
	case "getRadioShowAnalytics":
		s.handleGetRadioShowAnalytics(w, r)
	case "getRadioShowSnapshots":
		s.handleGetRadioShowSnapshots(w, r)
	case "getRadioShowListenerDetails":
		s.handleGetRadioShowListenerDetails(w, r)
	case "getAdminAnalytics":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleGetAdminAnalytics(w, r)
	case "sendNotification", "getSentNotifications", "getLinkMetadata", "deleteNotification", "clearSentNotifications", "deleteNotifications":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		switch name {
		case "sendNotification":
			s.handleSendNotification(w, r)
		case "getSentNotifications":
			s.handleGetSentNotifications(w, r)
		case "getLinkMetadata":
			s.handleGetLinkMetadata(w, r)
		case "deleteNotification":
			s.handleDeleteNotification(w, r)
		case "clearSentNotifications":
			s.handleClearSentNotifications(w, r)
		case "deleteNotifications":
			s.handleDeleteNotifications(w, r)
		}
	case "syncAzuraCastHistory":
		s.handleSyncRadioHistory(w, r)
	case "syncAzuraCastReports":
		s.handleSyncRadioReports(w, r)
	case "snapshotAzuraCastListenersManual":
		s.handleSnapshotRadioListeners(w, r)
	case "snapshotAzuraCastNowPlaying":
		s.handleSnapshotRadioNowPlaying(w, r)
	case "getEPGData":
		s.handleGetEPGData(w, r)
	case "getAdminEPGData":
		s.handleGetAdminEPGData(w, r)
	case "getEvents":
		s.handleGetEvents(w, r)
	case "epgHealthCheck":
		s.handleEpgHealthCheck(w, r)
	case "triggerEPGSync", "scheduleEPGCacheRefresh":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleTriggerEPGSync(w, r)
	case "invalidateEPGCache":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		s.handleInvalidateEPGCache(w, r)
	case "addEPGProgram", "updateEPGProgram", "deleteEPGProgram", "addEvent", "updateEvent", "deleteEvent":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		switch name {
		case "addEPGProgram":
			s.handleAddEPGProgram(w, r)
		case "updateEPGProgram":
			s.handleUpdateEPGProgram(w, r)
		case "deleteEPGProgram":
			s.handleDeleteEPGProgram(w, r)
		case "addEvent":
			s.handleAddEvent(w, r)
		case "updateEvent":
			s.handleUpdateEvent(w, r)
		case "deleteEvent":
			s.handleDeleteEvent(w, r)
		}
	case "getOnDemandData", "getOnDemandShowById", "getOnDemandSeasonEpisodes",
		"getPublicOnDemandData", "ondemandHealthCheck", "getSignedPlaylist":
		switch name {
		case "getOnDemandData":
			s.handleGetOnDemandData(w, r)
		case "getOnDemandShowById":
			s.handleGetOnDemandShowById(w, r)
		case "getOnDemandSeasonEpisodes":
			s.handleGetOnDemandSeasonEpisodes(w, r)
		case "getPublicOnDemandData":
			s.handleGetPublicOnDemandData(w, r)
		case "getSignedPlaylist":
			s.handleGetSignedPlaylist(w, r)
		case "ondemandHealthCheck":
			s.handleOndemandHealthCheck(w, r)
		}
	case "createOnDemandShow", "updateOnDemandShow", "deleteOnDemandShow",
		"createOnDemandSeason", "updateOnDemandSeason", "deleteOnDemandSeason",
		"updateOnDemandEpisode", "deleteOnDemandEpisode",
		"createSfxEpisode", "getEpisodePlaybackUrl", "uploadShowPoster",
		"refreshOnDemandCache", "scheduleOnDemandCacheRefresh":
		if !s.isServiceKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Unauthorized: Admin access required"})
			return
		}
		switch name {
		case "createOnDemandShow":
			s.handleCreateOnDemandShow(w, r)
		case "updateOnDemandShow":
			s.handleUpdateOnDemandShow(w, r)
		case "deleteOnDemandShow":
			s.handleDeleteOnDemandShow(w, r)
		case "createOnDemandSeason":
			s.handleCreateOnDemandSeason(w, r)
		case "updateOnDemandSeason":
			s.handleUpdateOnDemandSeason(w, r)
		case "deleteOnDemandSeason":
			s.handleDeleteOnDemandSeason(w, r)
		case "updateOnDemandEpisode":
			s.handleUpdateOnDemandEpisode(w, r)
		case "deleteOnDemandEpisode":
			s.handleDeleteOnDemandEpisode(w, r)
		case "createSfxEpisode":
			s.handleCreateSfxEpisode(w, r)
		case "getEpisodePlaybackUrl":
			s.handleGetEpisodePlaybackUrl(w, r)
		case "uploadShowPoster":
			s.handleUploadShowPoster(w, r)
		case "refreshOnDemandCache", "scheduleOnDemandCacheRefresh":
			if err := s.refreshOndemandCache(r.Context()); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "On-demand cache refreshed", "timestamp": time.Now().UTC().Format(time.RFC3339)})
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "function '" + name + "' not found"})
	}
}

// publicFn lists functions callable without an API key (mirrors the source
// functions, which were unauthenticated onRequest/onCall).
func (s *server) publicFn(name string) bool {
	switch name {
	case "batchTrackContentViews",
		"batchTrackContentImpressions",
		"batchTrackWatchProgress",
		"batchTrackContentSessions",
		"getAnalyticsMetrics",
		"getFirebaseAnalytics",
		"getRadioHistory",
		"getRadioReports",
		"getRadioCountryDetails",
		"getRadioShowAnalytics",
		"getRadioShowSnapshots",
		"getRadioShowListenerDetails",
		"syncAzuraCastHistory",
		"syncAzuraCastReports",
		"snapshotAzuraCastListenersManual",
		"snapshotAzuraCastNowPlaying",
		"getEPGData",
		"getAdminEPGData",
		"getEvents",
		"epgHealthCheck",
		"getOnDemandData",
		"getOnDemandShowById",
		"getOnDemandSeasonEpisodes",
		"getPublicOnDemandData",
		"getSignedPlaylist",
		"ondemandHealthCheck":
		return true
	}
	return false
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	out := map[string]any{
		"status":  "ok",
		"service": "salt-gofn",
		"version": "0.1.0",
		"time":    time.Now().UTC().Format(time.RFC3339),
	}
	body, hdr, err := s.doRest(ctx, "tv_shows", url.Values{"select": {"id"}, "limit": {"1"}})
	if err != nil {
		out["db_status"] = "error"
		out["db_error"] = err.Error()
	} else {
		out["db_status"] = "ok"
		out["db_bytes"] = len(body)
		var total int
		if _, serr := fmt.Sscanf(hdr.Get("Content-Range"), "0-0/%d", &total); serr == nil {
			out["tv_shows"] = total
		}
	}
	if s.tsdb != nil {
		adDB, aerr := s.tsdbDB(ctx)
		if aerr != nil {
			out["tsdb_status"] = "unavailable"
			out["tsdb_error"] = aerr.Error()
		} else if err := adDB.PingContext(ctx); err != nil {
			out["tsdb_status"] = "unavailable"
			out["tsdb_error"] = err.Error()
		} else {
			out["tsdb_status"] = "ok"
		}
	} else {
		out["tsdb_status"] = "disabled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": out})
}

func (s *server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	var p catalogParams
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		p.Q = q.Get("q")
		p.Type = q.Get("type")
		p.Limit, _ = strconv.Atoi(q.Get("limit"))
		p.Offset, _ = strconv.Atoi(q.Get("offset"))
	default:
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 100 {
		p.Limit = 100
	}

	// Search via OpenSearch when configured (replica-indexed catalog). Fall
	// back to PostgREST ilike if OpenSearch is unreachable.
	if s.osClient != nil {
		rows, count, err := s.osSearchCatalog(r.Context(), p)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"rows": rows, "count": count, "limit": p.Limit, "offset": p.Offset, "search": "opensearch"},
			})
			return
		}
		log.Printf("opensearch catalog search failed (%v); falling back to PostgREST", err)
	}

	vals := url.Values{
		"select": {"id,title,type,description,published,season_count,poster_url_16x9"},
	}
	if p.Q != "" {
		vals.Set("title", "ilike.*"+p.Q+"*")
	}
	if p.Type != "" {
		vals.Set("type", "eq."+p.Type)
	}
	vals.Set("limit", strconv.Itoa(p.Limit))
	vals.Set("offset", strconv.Itoa(p.Offset))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	body, hdr, err := s.doRest(ctx, "tv_shows", vals)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "bad response: " + err.Error()})
		return
	}
	total := len(rows)
	var cr int
	if _, serr := fmt.Sscanf(hdr.Get("Content-Range"), "0-0/%d", &cr); serr == nil {
		total = cr
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"rows": rows, "count": total, "limit": p.Limit, "offset": p.Offset, "search": "postgrest"},
	})
}

// osSearchCatalog queries OpenSearch (saltmedia-tv_shows) with fuzzy
// multi-field matching and returns the result rows plus total count.
func (s *server) osSearchCatalog(ctx context.Context, p catalogParams) ([]map[string]any, int, error) {
	var query map[string]any
	switch {
	case p.Q != "" && p.Type != "":
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": p.Q, "fields": []string{"title", "description"}, "fuzziness": "AUTO"}},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"type": p.Type}},
				},
			},
		}
	case p.Q != "":
		query = map[string]any{
			"multi_match": map[string]any{"query": p.Q, "fields": []string{"title", "description"}, "fuzziness": "AUTO"},
		}
	case p.Type != "":
		query = map[string]any{"term": map[string]any{"type": p.Type}}
	default:
		query = map[string]any{"match_all": map[string]any{}}
	}
	body, _ := json.Marshal(map[string]any{
		"query": query,
		"from":  p.Offset,
		"size":  p.Limit,
		"sort":  []any{map[string]any{"_score": "desc"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.osURL+"/saltmedia-tv_shows/_search", bytes.NewReader(body))
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

// handleGetUsersPaginated: admin user list with optional search.
// Backed by OpenSearch (fuzzy) when a searchTerm is given, otherwise paged
// PostgREST. Response shape matches the admin dashboard's
// GetUsersPaginatedResponse: { users, total, page, limit, nextPageToken }.
func (s *server) handleGetUsersPaginated(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term := q.Get("searchTerm")
	hasEmail := q.Get("hasEmail")
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	write := func(users []map[string]any, total int) {
		writeJSON(w, http.StatusOK, map[string]any{
			"users": users, "total": total, "page": page, "limit": limit,
			"nextPageToken": nil,
		})
	}

	// Search path: OpenSearch fuzzy on name/email/user_name.
	if term != "" && s.osClient != nil {
		rows, total, err := s.osSearchUsers(ctx, term, limit, offset)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		write(rows, total)
		return
	}

	// No search: page via PostgREST (service role).
	vals := url.Values{}
	vals.Set("select", "id,name,email,user_name,role,is_admin,is_blocked,is_verified,is_anonymous,profile_image_url,created_at")
	if hasEmail == "true" {
		vals.Set("email", "not.is.null")
	} else if hasEmail == "false" {
		vals.Set("email", "is.null")
	}
	vals.Set("order", "created_at.desc")
	vals.Set("limit", strconv.Itoa(limit))
	vals.Set("offset", strconv.Itoa(offset))

	body, hdr, err := s.doRest(ctx, "users", vals)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad response: " + err.Error()})
		return
	}
	total := len(rows)
	var cr int
	if _, serr := fmt.Sscanf(hdr.Get("Content-Range"), "0-0/%d", &cr); serr == nil {
		total = cr
	}
	write(rows, total)
}

// handleSearchUsers: admin user search backed by OpenSearch saltmedia-users.
// Requires the service-role key (admin dashboard uses /api/v1).
func (s *server) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	term := qv.Get("q")
	limit, _ := strconv.Atoi(qv.Get("limit"))
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if s.osClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "search unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, total, err := s.osSearchUsers(ctx, term, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"rows": rows, "count": total, "limit": limit, "offset": offset},
	})
}

// handleSearchArticles: search Joomla site articles via OpenSearch
// saltmedia-joomla_articles. Public/anon accessible (articles are public
// content). Fuzzy multi-field on title/body, optional category filter.
func (s *server) handleSearchArticles(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := qv.Get("q")
	cat := qv.Get("category")
	limit, _ := strconv.Atoi(qv.Get("limit"))
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if s.osClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "search unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, total, err := s.osSearchArticles(ctx, q, cat, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"rows": rows, "count": total, "limit": limit, "offset": offset},
	})
}

// osSearchArticles queries OpenSearch saltmedia-joomla_articles.
func (s *server) osSearchArticles(ctx context.Context, q, cat string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	switch {
	case q != "" && cat != "":
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
	case q != "":
		query = map[string]any{
			"multi_match": map[string]any{"query": q, "fields": []string{"title", "body"}, "fuzziness": "AUTO"},
		}
	case cat != "":
		query = map[string]any{"term": map[string]any{"category": cat}}
	default:
		query = map[string]any{"match_all": map[string]any{}}
	}
	body, _ := json.Marshal(map[string]any{
		"query": query,
		"from":  offset,
		"size":  limit,
		"sort":  []any{map[string]any{"_score": "desc"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.osURL+"/saltmedia-joomla_articles/_search", bytes.NewReader(body))
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

// osSearchUsers queries OpenSearch saltmedia-users with fuzzy multi-field
// matching on name/email/user_name.
func (s *server) osSearchUsers(ctx context.Context, q string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	if q == "" {
		query = map[string]any{"match_all": map[string]any{}}
	} else {
		query = map[string]any{
			"multi_match": map[string]any{
				"query":     q,
				"fields":    []string{"name", "email", "user_name"},
				"fuzziness": "AUTO",
			},
		}
	}
	body, _ := json.Marshal(map[string]any{
		"query": query,
		"from":  offset,
		"size":  limit,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.osURL+"/saltmedia-users/_search", bytes.NewReader(body))
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

// handleSearchOnDemand: OpenSearch search for on-demand content.
//   - `showId` present  → search episodes within that show (saltmedia-episodes).
//   - `showId` absent   → search published shows (saltmedia-tv_shows).
//
// Response: { success, data: { shows|episodes: [...], count, limit, offset } }.
// Public/anon accessible (on-demand content is public).
func (s *server) handleSearchOnDemand(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := qv.Get("q")
	showID := qv.Get("showId")
	limit, _ := strconv.Atoi(qv.Get("limit"))
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if s.osClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "search unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if showID != "" {
		rows, total, err := s.osSearchEpisodes(ctx, q, showID, limit, offset)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"data":    map[string]any{"episodes": rows, "count": total, "limit": limit, "offset": offset},
		})
		return
	}

	rows, total, err := s.osSearchShows(ctx, q, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"shows": rows, "count": total, "limit": limit, "offset": offset},
	})
}

// osSearch runs an OpenSearch search against the given index and returns the
// source rows plus total hit count.
func (s *server) osSearch(ctx context.Context, index string, query map[string]any, limit, offset int) ([]map[string]any, int, error) {
	body, _ := json.Marshal(map[string]any{
		"query": query,
		"from":  offset,
		"size":  limit,
		"sort":  []any{map[string]any{"_score": "desc"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.osURL+"/"+index+"/_search", bytes.NewReader(body))
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

// osSearchShows queries saltmedia-tv_shows for published shows, phrase-prefix
// matching on title/description (search-as-you-type friendly: "conne" → Connected).
func (s *server) osSearchShows(ctx context.Context, q string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	if q == "" {
		query = map[string]any{"term": map[string]any{"published": true}}
	} else {
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": q, "fields": []string{"title", "description"}, "type": "phrase_prefix"}},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"published": true}},
				},
			},
		}
	}
	return s.osSearch(ctx, "saltmedia-tv_shows", query, limit, offset)
}

// osSearchEpisodes queries saltmedia-episodes for episodes within a show,
// phrase-prefix matching on title/description.
func (s *server) osSearchEpisodes(ctx context.Context, q, showID string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	if q == "" {
		query = map[string]any{"term": map[string]any{"show_id": showID}}
	} else {
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": q, "fields": []string{"title", "description"}, "type": "phrase_prefix"}},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"show_id": showID}},
				},
			},
		}
	}
	return s.osSearch(ctx, "saltmedia-episodes", query, limit, offset)
}

// handleSearchBible: OpenSearch search over Bible verses (saltmedia-bible_verses).
// `version` is required (scoped to the reader's selected translation); `book`
// optionally narrows to a single book. Phrase-prefix matching so
// search-as-you-type works ("for god so loved", "romans").
// Response: { success, data: { verses: [...], count, limit, offset } }.
func (s *server) handleSearchBible(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := qv.Get("q")
	version := qv.Get("version")
	book := qv.Get("book")
	limit, _ := strconv.Atoi(qv.Get("limit"))
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "version is required"})
		return
	}
	if s.osClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "search unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, total, err := s.osSearchBible(ctx, q, version, book, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"verses": rows, "count": total, "limit": limit, "offset": offset},
	})
}

// osSearchBible queries saltmedia-bible_verses with phrase-prefix matching on
// verse text + book display name, filtered by version (and optional book).
func (s *server) osSearchBible(ctx context.Context, q, version, book string, limit, offset int) ([]map[string]any, int, error) {
	var query map[string]any
	if q == "" {
		query = map[string]any{"term": map[string]any{"version": version}}
	} else {
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": q, "fields": []string{"text", "book_display"}, "type": "phrase_prefix"}},
				},
				"filter": []any{
					map[string]any{"term": map[string]any{"version": version}},
				},
			},
		}
	}
	if book != "" {
		if bq, ok := query["bool"].(map[string]any); ok {
			filters, _ := bq["filter"].([]any)
			bq["filter"] = append(filters, map[string]any{"term": map[string]any{"book": book}})
		}
	}
	return s.osSearch(ctx, "saltmedia-bible_verses", query, limit, offset)
}

// handleSearchEpg: OpenSearch search over EPG programs (saltmedia-epg_programs).
// Phrase-prefix matching on program name / presenter / genre / details so
// search-as-you-type works. Optional `stationId` narrows to one station.
// Response: { success, data: { programs: [...], count, limit, offset } }.
func (s *server) handleSearchEpg(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := qv.Get("q")
	stationID := qv.Get("stationId")
	limit, _ := strconv.Atoi(qv.Get("limit"))
	offset, _ := strconv.Atoi(qv.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if s.osClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "search unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, total, err := s.osSearchEpg(ctx, q, stationID, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"programs": rows, "count": total, "limit": limit, "offset": offset},
	})
}

// osSearchEpg queries saltmedia-epg_programs with phrase-prefix matching.
func (s *server) osSearchEpg(ctx context.Context, q, stationID string, limit, offset int) ([]map[string]any, int, error) {
	query := map[string]any{}
	if q != "" {
		query = map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"multi_match": map[string]any{"query": q, "fields": []string{"program_name", "presenter", "details"}, "type": "phrase_prefix"}},
				},
			},
		}
	}
	if stationID != "" {
		if bq, ok := query["bool"].(map[string]any); ok {
			filters, _ := bq["filter"].([]any)
			bq["filter"] = append(filters, map[string]any{"term": map[string]any{"station_id": stationID}})
		} else {
			query = map[string]any{"bool": map[string]any{"filter": []any{map[string]any{"term": map[string]any{"station_id": stationID}}}}}
		}
	}
	return s.osSearch(ctx, "saltmedia-epg_programs", query, limit, offset)
}

func (s *server) doRest(ctx context.Context, path string, values url.Values) ([]byte, http.Header, error) {
	u := s.restURL + "/" + path
	if len(values) > 0 {
		u += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, errors.New(string(body))
	}
	return body, resp.Header, nil
}

func (s *server) authorized(r *http.Request) bool {
	if s.serviceKey == "" && s.anonKey == "" {
		return true
	}
	key := r.Header.Get("apikey")
	if key == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return key != "" && (key == s.serviceKey || key == s.anonKey)
}

// isServiceKey reports whether the request carries the privileged service-role
// key — the mesh analog of Firebase's admin claim (context.auth.token.admin).
func (s *server) isServiceKey(r *http.Request) bool {
	if s.serviceKey == "" {
		return false
	}
	key := r.Header.Get("apikey")
	if key == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return key == s.serviceKey
}

func fnName(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
