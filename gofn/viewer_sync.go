package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

func (s *server) syncViewerDay(ctx context.Context, day string) error {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO public.viewer_daily (day, device_type, os, browser, country, isp, requests, distinct_sessions, updated_at)
		SELECT 
			$1::date, 
			coalesce(nullif(device_type, ''), 'mobile'), 
			coalesce(nullif(os, ''), 'Other'), 
			coalesce(nullif(browser, ''), 'Other'), 
			coalesce(nullif(country_code, ''), 'UG'), 
			'Broadband Provider', 
			count(*), 
			count(distinct coalesce(nullif(user_id, ''), nullif(device_id, ''))), 
			now()
		FROM public.content_views
		WHERE received_at::date = $1::date
		GROUP BY 1, 2, 3, 4, 5, 6
		ON CONFLICT (day, device_type, os, browser, country, isp)
		DO UPDATE SET requests = EXCLUDED.requests, distinct_sessions = EXCLUDED.distinct_sessions, updated_at = now()`, day)
	return err
}

// backfillViewerDaily syncs the last N days so the dashboard
// has history before the hourly live sync takes over.
func (s *server) backfillViewerDaily(ctx context.Context, days int) {
	for i := days; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if err := s.syncViewerDay(ctx, day); err != nil {
			log.Printf("viewer backfill %s: %v", day, err)
		} else {
			log.Printf("viewer backfill %s: ok", day)
		}
	}
}

// handleSyncViewerDaily allows manual triggering of viewer daily sync from BigQuery.
func (s *server) handleSyncViewerDaily(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	s.backfillViewerDaily(ctx, days)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "synced_days": days})
}
