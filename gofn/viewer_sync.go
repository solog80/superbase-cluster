package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// Viewer device/OS/browser/country daily aggregates.
//
// The CDN viewer-request log lives in BigQuery (viewer_logs.viewer_requests_native:
// user_agent + country per request, ~2.4M rows/day). The mesh aggregates each
// day IN BigQuery (so only a tiny result crosses the wire) and upserts it into
// the TSDB viewer_daily table, which get_firebase_analytics reads for the
// User & Device device/OS/browser/geographic breakdowns.

const viewerDailyQuery = `
SELECT
  '{day}' AS day,
  CASE
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)iPhone|iPad|iPod') THEN 'mobile'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Android|Dalvik') THEN 'mobile'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Windows') THEN 'desktop'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Macintosh|Mac OS') THEN 'desktop'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Linux') THEN 'desktop'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)TV|SmartTV|Smart TV') THEN 'tv'
    ELSE 'other'
  END AS device_type,
  CASE
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)iPhone|iPad|iPod') THEN 'iOS'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Android|Dalvik') THEN 'Android'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Windows') THEN 'Windows'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Macintosh|Mac OS') THEN 'macOS'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Linux') THEN 'Linux'
    ELSE 'Other'
  END AS os,
  CASE
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)AppleCoreMedia') THEN 'iOS Native'
    WHEN REGEXP_CONTAINS(user_agent, r'(?i)Dalvik') THEN 'Android Native'
    WHEN REGEXP_CONTAINS(user_agent, r'Chrome/') THEN 'Chrome'
    WHEN REGEXP_CONTAINS(user_agent, r'Firefox/') THEN 'Firefox'
    WHEN REGEXP_CONTAINS(user_agent, r'Safari/') THEN 'Safari'
    ELSE 'Other'
  END AS browser,
  COALESCE(NULLIF(country, ''), 'Unknown') AS country,
  COUNT(*) AS requests,
  COUNT(DISTINCT COALESCE(NULLIF(session, ''), client_ip)) AS distinct_sessions
FROM ` + "`salt-media-app1.viewer_logs.viewer_requests_native`" + `
WHERE DATE(ts) = '{day}' AND COALESCE(is_datacenter, false) = false
GROUP BY day, device_type, os, browser, country`

// syncViewerDay aggregates one day of CDN viewer requests from BigQuery into
// the TSDB viewer_daily table (upsert by natural key).
func (s *server) syncViewerDay(ctx context.Context, day string) error {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return err
	}
	rows, err := s.bigQueryQuery(ctx, strings.ReplaceAll(viewerDailyQuery, "{day}", day))
	if err != nil {
		return err
	}
	for _, r := range rows {
		requests, _ := strconv.ParseInt(str(r["requests"]), 10, 64)
		sessionsN, _ := strconv.ParseInt(str(r["distinct_sessions"]), 10, 64)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO public.viewer_daily (day, device_type, os, browser, country, requests, distinct_sessions, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7, now())
			ON CONFLICT (day, device_type, os, browser, country)
			DO UPDATE SET requests = EXCLUDED.requests, distinct_sessions = EXCLUDED.distinct_sessions, updated_at = now()`,
			day, str(r["device_type"]), str(r["os"]), str(r["browser"]), str(r["country"]), requests, sessionsN); err != nil {
			return err
		}
	}
	return nil
}

// backfillViewerDaily syncs the last N days (excluding today) so the dashboard
// has history before the hourly live sync takes over.
func (s *server) backfillViewerDaily(ctx context.Context, days int) {
	for i := days; i >= 1; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if err := s.syncViewerDay(ctx, day); err != nil {
			log.Printf("viewer backfill %s: %v", day, err)
		} else {
			log.Printf("viewer backfill %s: ok", day)
		}
	}
}
