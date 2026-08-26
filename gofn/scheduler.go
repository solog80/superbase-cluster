package main

import (
	"context"
	"log"
	"time"
)

// Scheduler replicates the Firebase pubsub schedules for the AzuraCast sync
// jobs (functions/src/azuracast.js):
//
//	snapshotAzuraCastNowPlayingScheduled   every minute
//	snapshotAzuraCastListenersScheduled    every 5 minutes
//	syncAzuraCastHistoryScheduled          hourly
//	syncAzuraCastReportsScheduled          every 6 hours
//
// These pull straight from the external AzuraCast API into TimescaleDB.
// Enabled when TSDB is configured and AZURACAST_API_KEY is present.

func (s *server) startScheduler() {
	if s.tsdb == nil {
		log.Println("scheduler: disabled (no TSDB config)")
		return
	}
	if getenv("AZURACAST_API_KEY", "") == "" {
		log.Println("scheduler: disabled (no AZURACAST_API_KEY)")
		return
	}
	log.Println("scheduler: starting radio sync jobs")

	go s.runEvery(time.Minute, func(ctx context.Context) {
		n, err := s.snapshotRadioNowPlaying(ctx, saltFMStation)
		if err != nil {
			log.Printf("scheduler nowplaying: %v", err)
			return
		}
		log.Printf("scheduler nowplaying: +%d", n)
	})

	go s.runEvery(5*time.Minute, func(ctx context.Context) {
		n, err := s.snapshotRadioListeners(ctx, saltFMStation)
		if err != nil {
			log.Printf("scheduler listeners: %v", err)
			return
		}
		log.Printf("scheduler listeners: +%d", n)
	})

	go s.runEvery(time.Hour, func(ctx context.Context) {
		n, err := s.syncRadioHistory(ctx, saltFMStation)
		if err != nil {
			log.Printf("scheduler history: %v", err)
			return
		}
		log.Printf("scheduler history: +%d", n)
	})

	go s.runEvery(6*time.Hour, func(ctx context.Context) {
		counts, err := s.syncRadioCharts(ctx, saltFMStation)
		if err != nil {
			log.Printf("scheduler reports: %v", err)
			return
		}
		log.Printf("scheduler reports: %v", counts)
	})

	// EPG cache warm + metadata sync (replaces scheduleEPGCacheRefresh, 12h).
	go s.runEvery(12*time.Hour, func(ctx context.Context) {
		// Warm today's EPG cache (the /getEPGData endpoint will cache it).
		s.epgMu.Lock()
		s.epgCache = map[string]epgCacheEntry{}
		s.epgMu.Unlock()
		// Mirror EPG metadata to TSDB for analytics thumbnails/station names.
		if n, err := s.syncEPGMetadata(ctx); err != nil {
			log.Printf("scheduler epg sync: %v", err)
		} else {
			log.Printf("scheduler epg sync: %d metadata rows", n)
		}
	})

	// On-demand cache warm (replaces scheduleOnDemandCacheRefresh, 6h).
	go s.runEvery(6*time.Hour, func(ctx context.Context) {
		if err := s.refreshOndemandCache(ctx); err != nil {
			log.Printf("scheduler ondemand cache: %v", err)
			return
		}
		log.Printf("scheduler ondemand cache: warmed")
	})
}

// runEvery runs fn immediately after a short warmup, then on the interval.
func (s *server) runEvery(interval time.Duration, fn func(context.Context)) {
	time.Sleep(2 * time.Second) // let the HTTP server + TSDB pool warm up
	for {
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		fn(ctx)
		cancel()
		time.Sleep(interval)
	}
}
