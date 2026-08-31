package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Direct Google Analytics for Firebase (GA4) Data API integration.
//
// The mesh calls analyticsdata.googleapis.com runReport with the Firebase
// service account (FCM_SERVICE_ACCOUNT) scoped to analytics.readonly. It feeds
// the "User & Device" dashboard's standard metrics (DAU/MAU, sessions, devices,
// OS, browser, country, daily trend); the app-specific pieces (churn reasons,
// content-type engagement, retention) still come from content_sessions.

const ga4Scope = "https://www.googleapis.com/auth/analytics.readonly"

func (s *server) ga4PropertyID() string {
	return getenv("GA4_PROPERTY_ID", "439334523")
}

// ga4Report is a single runReport result: column names + scalar rows.
type ga4Report struct {
	dims    []string
	metrics []string
	rows    [][]string
	totals  []string
}

// ga4RunReport calls the GA4 Data API. dims/metrics are GA4 dimension/metric
// names (e.g. "date", "deviceCategory", "activeUsers"). All GA4 values arrive
// as strings.
func (s *server) ga4RunReport(ctx context.Context, startDate, endDate string, dims, metrics []string, limit int) (*ga4Report, error) {
	sa, err := loadFCMSA()
	if err != nil {
		return nil, err
	}
	token, err := s.serviceAccountToken(ctx, sa, ga4Scope)
	if err != nil {
		return nil, err
	}
	reqBody := map[string]any{
		"dateRanges": []any{map[string]any{"startDate": startDate, "endDate": endDate}},
		"metrics":    make([]any, 0, len(metrics)),
	}
	for _, m := range metrics {
		reqBody["metrics"] = append(reqBody["metrics"].([]any), map[string]any{"name": m})
	}
	if len(dims) > 0 {
		ds := make([]any, 0, len(dims))
		for _, d := range dims {
			ds = append(ds, map[string]any{"name": d})
		}
		reqBody["dimensions"] = ds
	}
	if limit > 0 {
		reqBody["limit"] = limit
	}
	body, _ := json.Marshal(reqBody)

	u := "https://analyticsdata.googleapis.com/v1beta/properties/" + s.ga4PropertyID() + ":runReport"
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ga4 %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		DimensionHeaders []struct {
			Name string `json:"name"`
		} `json:"dimensionHeaders"`
		MetricHeaders []struct {
			Name string `json:"name"`
		} `json:"metricHeaders"`
		Rows []struct {
			DimensionValues []struct {
				Value string `json:"value"`
			} `json:"dimensionValues"`
			MetricValues []struct {
				Value string `json:"value"`
			} `json:"metricValues"`
		} `json:"rows"`
		Totals []struct {
			MetricValues []struct {
				Value string `json:"value"`
			} `json:"metricValues"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	rep := &ga4Report{}
	for _, h := range out.DimensionHeaders {
		rep.dims = append(rep.dims, h.Name)
	}
	for _, h := range out.MetricHeaders {
		rep.metrics = append(rep.metrics, h.Name)
	}
	for _, r := range out.Rows {
		row := make([]string, 0, len(r.DimensionValues)+len(r.MetricValues))
		for _, d := range r.DimensionValues {
			row = append(row, d.Value)
		}
		for _, m := range r.MetricValues {
			row = append(row, m.Value)
		}
		rep.rows = append(rep.rows, row)
	}
	if len(out.Totals) > 0 {
		for _, m := range out.Totals[0].MetricValues {
			rep.totals = append(rep.totals, m.Value)
		}
	}
	return rep, nil
}

// metric returns the numeric value (string) of a named metric from totals.
func (rep *ga4Report) metric(name string) string {
	for i, m := range rep.metrics {
		if m == name {
			if len(rep.totals) > i {
				return rep.totals[i]
			}
			// GA4 puts no-dimension aggregates in rows (metric values only).
			if len(rep.rows) > 0 && i < len(rep.rows[0]) {
				return rep.rows[0][i]
			}
		}
	}
	return ""
}

// pairs returns (dimValue, metricValue) for the given dimension/metric names.
// Row layout is [dimensionValues..., metricValues...], so metric columns start
// at len(dims).
func (rep *ga4Report) pairs(dim, metric string) [][2]string {
	di, mi := -1, -1
	for i, d := range rep.dims {
		if d == dim {
			di = i
		}
	}
	for i, m := range rep.metrics {
		if m == metric {
			mi = i
		}
	}
	if mi >= 0 {
		mi += len(rep.dims)
	}
	var out [][2]string
	if di < 0 || mi < 0 {
		return out
	}
	for _, r := range rep.rows {
		if di < len(r) && mi < len(r) {
			out = append(out, [2]string{r[di], r[mi]})
		}
	}
	return out
}

// breakdown builds the dashboard breakdown rows (name, users, sessions,
// percentage of users) from a (dimValue, activeUsers) report.
func breakdown(rep *ga4Report, nameKey string, dim string) []any {
	pairs := rep.pairs(dim, "activeUsers")
	usersTotal := 0
	for _, p := range pairs {
		usersTotal += atoi(p[1])
	}
	// sessions per dim from the same report's "sessions" metric.
	sessByDim := map[string]int{}
	for _, r := range rep.rows {
		di, mi := -1, -1
		for i, d := range rep.dims {
			if d == dim {
				di = i
			}
		}
		for i, m := range rep.metrics {
			if m == "sessions" {
				mi = i
			}
		}
		if mi >= 0 {
			mi += len(rep.dims)
		}
		if di >= 0 && mi >= 0 && di < len(r) && mi < len(r) {
			sessByDim[r[di]] = atoi(r[mi])
		}
	}
	out := make([]any, 0, len(pairs))
	for _, p := range pairs {
		users := atoi(p[1])
		pct := 0.0
		if usersTotal > 0 {
			pct = float64(users) * 100.0 / float64(usersTotal)
		}
		out = append(out, map[string]any{
			nameKey:              p[0],
			"users":              users,
			"sessions":           sessByDim[p[0]],
			"avgSessionDuration": 0,
			"percentage":         round2(pct),
		})
	}
	return out
}

// buildGA4Payload assembles the User & Device dashboard payload from GA4
// (standard metrics) merged with content_sessions (app-specific metrics).
func (s *server) buildGA4Payload(ctx context.Context, startDate, endDate string) (map[string]any, error) {
	overview, err := s.ga4RunReport(ctx, startDate, endDate, nil, []string{"activeUsers", "newUsers", "sessions", "averageSessionDuration"}, 0)
	if err != nil {
		return nil, err
	}
	dauRep, err := s.ga4RunReport(ctx, endDate, endDate, nil, []string{"activeUsers"}, 0)
	if err != nil {
		return nil, err
	}
	trendRep, err := s.ga4RunReport(ctx, startDate, endDate, []string{"date"}, []string{"activeUsers", "sessions"}, 0)
	if err != nil {
		return nil, err
	}
	devRep, err := s.ga4RunReport(ctx, startDate, endDate, []string{"deviceCategory"}, []string{"activeUsers", "sessions"}, 0)
	if err != nil {
		return nil, err
	}
	osRep, err := s.ga4RunReport(ctx, startDate, endDate, []string{"operatingSystem"}, []string{"activeUsers", "sessions"}, 10)
	if err != nil {
		return nil, err
	}
	browserRep, err := s.ga4RunReport(ctx, startDate, endDate, []string{"browser"}, []string{"activeUsers", "sessions"}, 10)
	if err != nil {
		return nil, err
	}
	geoRep, err := s.ga4RunReport(ctx, startDate, endDate, []string{"country"}, []string{"activeUsers", "sessions"}, 15)
	if err != nil {
		return nil, err
	}

	// App-specific metrics (churn, content-type, retention) from content_sessions.
	var cs map[string]any
	if payload, err := s.radioRPCPayload(ctx, "get_firebase_analytics", []string{"p_start", "p_end"},
		map[string]any{"p_start": startDate, "p_end": endDate}, metricsCacheTTL); err == nil {
		_ = json.Unmarshal(payload, &cs)
	}

	trend := make([]any, 0, len(trendRep.rows))
	for _, r := range trendRep.rows {
		if len(r) >= 3 {
			trend = append(trend, map[string]any{
				"date": formatGa4Date(r[0]), "dau": atoi(r[1]), "sessions": atoi(r[2]),
			})
		}
	}
	// GA4 returns date rows in arbitrary order; sort ascending so the chart
	// axis is chronological.
	sort.Slice(trend, func(i, j int) bool {
		return trend[i].(map[string]any)["date"].(string) < trend[j].(map[string]any)["date"].(string)
	})

	retention := any(map[string]any{"day1": 0, "day7": 0, "day30": 0})
	if v := csOr(cs, "retention"); v != nil {
		retention = v
	}
	engagement := any(map[string]any{"avgSessionDuration": 0, "maxSessionDuration": 0, "avgTimePerUser": 0})
	if v := csOr(cs, "engagement"); v != nil {
		engagement = v
	}

	return map[string]any{
		"period":    "custom",
		"startDate": startDate,
		"endDate":   endDate,
		"overview": map[string]any{
			"dau":                atoi(dauRep.metric("activeUsers")),
			"mau":                atoi(overview.metric("activeUsers")),
			"newUsers":           atoi(overview.metric("newUsers")),
			"totalSessions":      atoi(overview.metric("sessions")),
			"uniqueUsers":        atoi(overview.metric("activeUsers")),
			"avgSessionDuration": atoi(overview.metric("averageSessionDuration")),
		},
		"devices":               map[string]any{"breakdown": breakdown(devRep, "device", "deviceCategory")},
		"operatingSystems":      map[string]any{"breakdown": breakdown(osRep, "os", "operatingSystem")},
		"browsers":              map[string]any{"breakdown": breakdown(browserRep, "browser", "browser")},
		"geographic":            map[string]any{"breakdown": breakdown(geoRep, "country", "country")},
		"retention":             retention,
		"engagement":            engagement,
		"churnReasons":          csOr(cs, "churnReasons"),
		"contentTypeEngagement": csOr(cs, "contentTypeEngagement"),
		"dauTrend":              trend,
		"timestamp":             time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}, nil
}

// handleGetGa4Analytics serves the User & Device dashboard from the GA4 Data
// API, falling back to the TimescaleDB RPC (content_sessions + viewer_daily)
// when GA4 is unreachable (e.g. service account not yet granted access).
func (s *server) handleGetGa4Analytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	startDate := strings.TrimSpace(q.Get("startDate"))
	endDate := strings.TrimSpace(q.Get("endDate"))
	if startDate == "" {
		startDate = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	}
	if endDate == "" {
		endDate = time.Now().UTC().Format("2006-01-02")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	payload, err := s.buildGA4Payload(ctx, startDate, endDate)
	if err != nil {
		log.Printf("ga4 analytics failed (%v); falling back to TSDB RPC", err)
		s.handleGetFirebaseAnalytics(w, r)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// ──────────────── small helpers ────────────────

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100.0
}

func csOr(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func formatGa4Date(s string) string {
	if len(s) == 8 {
		return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	return s
}
