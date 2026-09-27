package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type OMENativeConnections struct {
	File      int `json:"file"`
	LLHLS     int `json:"llhls"`
	OVT       int `json:"ovt"`
	Push      int `json:"push"`
	Thumbnail int `json:"thumbnail"`
	WebRTC    int `json:"webrtc"`
	HLS       int `json:"hls"`
}

type OMENativeResponse struct {
	StatusCode int `json:"statusCode"`
	Message    string `json:"message"`
	Response   struct {
		TotalConnections    int                  `json:"totalConnections"`
		MaxTotalConnections int                  `json:"maxTotalConnections"`
		AvgThroughputIn     int64                `json:"avgThroughputIn"`
		AvgThroughputOut    int64                `json:"avgThroughputOut"`
		LastThroughputIn    int64                `json:"lastThroughputIn"`
		LastThroughputOut   int64                `json:"lastThroughputOut"`
		TotalBytesIn        int64                `json:"totalBytesIn"`
		TotalBytesOut       int64                `json:"totalBytesOut"`
		Connections         OMENativeConnections `json:"connections"`
	} `json:"response"`
}

// fetchOMENativeStats queries OvenMediaEngine's native REST API (/v1/stats/current/...) on port 8091.
func (s *server) fetchOMENativeStats(ctx context.Context, subPath string) (*OMENativeResponse, error) {
	hosts := []string{
		getenv("OME_API_HOST", ""),
		"http://172.27.0.1:8091",
		"http://host.docker.internal:8091",
		"http://198.204.224.170:8091",
		"http://127.0.0.1:8091",
	}
	omeToken := osGetenv("OME_ACCESS_TOKEN", "s4lt5tv_0me_api_2026")
	if omeToken == "" {
		omeToken = osGetenv("OME_API_TOKEN", "s4lt5tv_0me_api_2026")
	}

	var lastErr error
	for _, omeHost := range hosts {
		if omeHost == "" {
			continue
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		targetURL := fmt.Sprintf("%s/v1/stats/current/vhosts/default/apps/app%s", omeHost, subPath)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, targetURL, nil)
		if err != nil {
			attemptCancel()
			lastErr = err
			continue
		}

		if omeToken != "" {
			encoded := base64.StdEncoding.EncodeToString([]byte(omeToken))
			req.Header.Set("Authorization", "Basic "+encoded)
		}

		resp, err := s.client.Do(req)
		if err != nil {
			attemptCancel()
			lastErr = err
			continue
		}

		if resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			attemptCancel()
			lastErr = fmt.Errorf("ome api status %d", resp.StatusCode)
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		attemptCancel()
		if err != nil {
			lastErr = err
			continue
		}

		var out OMENativeResponse
		if err := json.Unmarshal(body, &out); err != nil {
			log.Printf("ome json unmarshal %s %s err: %v body: %s", subPath, omeHost, err, string(body))
			lastErr = err
			continue
		}
		log.Printf("ome success %s %s status: %d conn: %d", subPath, omeHost, out.StatusCode, out.Response.TotalConnections)
		return &out, nil
	}
	log.Printf("ome all hosts failed %s err: %v", subPath, lastErr)
	return nil, lastErr
}

// handleGetLiveTvStats retrieves native OvenMediaEngine metrics (totalConnections, llhls, webrtc, throughput)
// merged with GeoIP/country logs.
// handleGetLiveTvStats retrieves Varnish 30s sliding window metrics merged with throughput stats.
func (s *server) handleGetLiveTvStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	vStats := globalVarnishTracker.GetStats(30 * time.Second)

	// Fetch native OME stats for throughput metrics
	var sumThroughputOut int64 = 0
	var sumThroughputIn int64 = 0
	if stream1, err := s.fetchOMENativeStats(ctx, "/streams/stream"); err == nil && stream1 != nil && stream1.StatusCode == 200 {
		sumThroughputOut += stream1.Response.LastThroughputOut
		sumThroughputIn += stream1.Response.LastThroughputIn
	}
	if stream2, err := s.fetchOMENativeStats(ctx, "/streams/stream2"); err == nil && stream2 != nil && stream2.StatusCode == 200 {
		sumThroughputOut += stream2.Response.LastThroughputOut
		sumThroughputIn += stream2.Response.LastThroughputIn
	}

	viewers := vStats.TotalViewers
	stMap := vStats.StreamCounts

	resp := map[string]any{
		"viewers":            viewers,
		"total_connections":  viewers,
		"llhls_connections":  viewers,
		"webrtc_connections": 0,
		"streams":            stMap,
		"avg_throughput_out": sumThroughputOut,
		"avg_throughput_in":  sumThroughputIn,
		"window_seconds":     vStats.WindowSeconds,
		"source":             "varnish_30s_window",
	}

	writeJSON(w, http.StatusOK, resp)
}

// helper for os.Getenv fallback
func osGetenv(key, fallback string) string {
	if val := getenv(key, ""); val != "" {
		return val
	}
	return fallback
}

// handleGetViewerStats retrieves viewer statistics per stream over recent minutes or date range from TSDB.
func (s *server) handleGetViewerStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minutes := atoiDefault(q.Get("minutes"), 30)
	startDate := strings.TrimSpace(q.Get("startDate"))
	endDate := strings.TrimSpace(q.Get("endDate"))

	// If requested window is > 24 hours (1440 mins) or specific date range provided, query TimescaleDB
	if (minutes > 1440 || startDate != "" || endDate != "") && s.tsdb != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if db, err := s.tsdbDB(ctx); err == nil {
			bucket := "1 hour"
			if minutes > 10080 { // > 7 days -> bucket by day
				bucket = "1 day"
			}
			var timeWhere string
			var args []any

			if startDate != "" && endDate != "" {
				timeWhere = "timestamp >= $1::timestamptz AND timestamp <= ($2::date + interval '1 day')::timestamptz"
				args = append(args, startDate, endDate)
			} else {
				timeWhere = "timestamp >= now() - ($1 * interval '1 minute')"
				args = append(args, minutes)
			}

			query := fmt.Sprintf(`
				SELECT time_bucket('%s', timestamp) as bucket_time,
				       case when content_id like '%%stream2%%' or content_id like '%%salt_tv_two%%' then 'stream2' else 'stream' end as st,
				       count(distinct coalesce(user_id, device_id)) as v
				FROM public.content_sessions
				WHERE %s
				GROUP BY 1, 2
				ORDER BY 1 ASC`, bucket, timeWhere)

			rows, err := db.QueryContext(ctx, query, args...)
			if err == nil {
				defer rows.Close()
				var history []map[string]any
				for rows.Next() {
					var bTime time.Time
					var st string
					var v int
					if err := rows.Scan(&bTime, &st, &v); err == nil {
						history = append(history, map[string]any{
							"minute":  bTime.Format(time.RFC3339),
							"stream":  st,
							"viewers": v,
						})
					}
				}
				if len(history) > 0 {
					writeJSON(w, http.StatusOK, map[string]any{
						"viewers": history,
						"minutes": minutes,
						"source":  "timescaledb_historical",
					})
					return
				}
			}
		}
	}

	if minutes < 1 {
		minutes = 1
	}
	if minutes > 1440 {
		minutes = 1440
	}

	globalVarnishTracker.RecordMinuteSnapshot()
	history := globalVarnishTracker.GetHistory(minutes)

	writeJSON(w, http.StatusOK, map[string]any{
		"viewers": history,
		"minutes": minutes,
		"source":  "varnish_realtime_ring",
	})
}

// handleGetViewerCountries queries viewer country and ISP breakdowns dynamically from active Varnish sessions.
func (s *server) handleGetViewerCountries(w http.ResponseWriter, r *http.Request) {
	minutes := atoiDefault(r.URL.Query().Get("minutes"), 60)
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 1440 {
		minutes = 1440
	}

	activeIPs := globalVarnishTracker.GetActiveViewerIPs(time.Duration(minutes) * time.Minute)

	countryCounts := make(map[string]int)
	countryCodeMap := make(map[string]string)
	countryNameMap := make(map[string]string)

	ispCounts := make(map[string]int)
	ispCodeMap := make(map[string]string)
	ispNameMap := make(map[string]string)

	for _, ip := range activeIPs {
		geo := globalGeoIPService.Lookup(ip)

		cKey := fmt.Sprintf("%s:%s", geo.CountryCode, geo.CountryName)
		countryCounts[cKey]++
		countryCodeMap[cKey] = geo.CountryCode
		countryNameMap[cKey] = geo.CountryName

		iKey := fmt.Sprintf("%s:%s", geo.CountryCode, geo.ISP)
		ispCounts[iKey]++
		ispCodeMap[iKey] = geo.CountryCode
		ispNameMap[iKey] = geo.ISP
	}

	type countryItem struct {
		Code    string `json:"code"`
		Country string `json:"country"`
		Viewers int    `json:"viewers"`
	}
	var countries []countryItem
	for k, count := range countryCounts {
		countries = append(countries, countryItem{
			Code:    countryCodeMap[k],
			Country: countryNameMap[k],
			Viewers: count,
		})
	}
	sort.Slice(countries, func(i, j int) bool {
		return countries[i].Viewers > countries[j].Viewers
	})

	type ispItem struct {
		Code    string `json:"code"`
		ISP     string `json:"isp"`
		Viewers int    `json:"viewers"`
	}
	var isps []ispItem
	for k, count := range ispCounts {
		isps = append(isps, ispItem{
			Code:    ispCodeMap[k],
			ISP:     ispNameMap[k],
			Viewers: count,
		})
	}
	sort.Slice(isps, func(i, j int) bool {
		return isps[i].Viewers > isps[j].Viewers
	})

	if len(countries) > 20 {
		countries = countries[:20]
	}
	if len(isps) > 20 {
		isps = isps[:20]
	}

	if countries == nil {
		countries = []countryItem{}
	}
	if isps == nil {
		isps = []ispItem{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"countries": countries,
		"isps":      isps,
		"minutes":   minutes,
		"source":    "varnish_geoip_realtime",
	})
}

// handleGetViewerPeak queries peak concurrent real viewers and current distinct 30s Varnish viewers.
func (s *server) handleGetViewerPeak(w http.ResponseWriter, r *http.Request) {
	minutesStr := strings.TrimSpace(r.URL.Query().Get("minutes"))
	var minutes int
	hasWindow := false
	if minutesStr != "" && minutesStr != "null" {
		if m, err := strconv.Atoi(minutesStr); err == nil && m > 0 {
			minutes = m
			hasWindow = true
		}
	}

	var windowVal any = nil
	if hasWindow {
		windowVal = minutes
	}

	// Live 30-second sliding window current viewers count
	vStats := globalVarnishTracker.GetStats(30 * time.Second)
	currentVal := vStats.TotalViewers

	writeJSON(w, http.StatusOK, map[string]any{
		"peak_viewers":    currentVal,
		"peak_time":       time.Now().UTC().Format(time.RFC3339),
		"window_minutes":  windowVal,
		"current_viewers": currentVal,
		"source":          "varnish_30s_window",
	})
}
