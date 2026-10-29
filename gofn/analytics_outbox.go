package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

var analyticsTables = map[string]bool{
	"content_views":       true,
	"content_impressions": true,
	"watch_progress":      true,
	"content_sessions":    true,
	"ad_events":           true,
}

func allowedAnalyticsTable(table string) bool {
	return analyticsTables[table]
}

// analyticsDedupeKey fingerprints the original client events, not the built
// rows, so an outbox retry with a new server timestamp still deduplicates.
func analyticsDedupeKey(table string, events []map[string]any) string {
	encoded, _ := json.Marshal(events)
	sum := sha256.Sum256(append([]byte(table+"\x00"), encoded...))
	return hex.EncodeToString(sum[:])
}

// insertRowsJSON inserts rows that came back from the outbox. Times are JSON
// strings by then, so convert known timestamp columns back to time.Time before
// handing them to pgx.
func (s *server) insertRowsJSON(ctx context.Context, table string, rows []map[string]any) error {
	if !allowedAnalyticsTable(table) {
		return fmt.Errorf("unsupported analytics table %q", table)
	}
	if len(rows) == 0 {
		return nil
	}
	normalizeAnalyticsTimes(rows)

	db, err := s.tsdbDB(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errTsdbUnavailable, err)
	}

	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	placeholders, args := insertPlaceholders(rows, cols)
	stmt := "INSERT INTO public." + table + " (" + joinCols(cols) + ") VALUES " + placeholders
	if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("insert %s: %w", table, err)
	}
	log.Printf("%s: inserted %d rows into timescale (outbox)", table, len(rows))
	return nil
}

func normalizeAnalyticsTimes(rows []map[string]any) {
	for _, row := range rows {
		for key, value := range row {
			if !isAnalyticsTimeColumn(key) {
				continue
			}
			switch typed := value.(type) {
			case time.Time:
				continue
			case string:
				if typed == "" {
					row[key] = nil
					continue
				}
				if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
					row[key] = parsed
					continue
				}
				if parsed, err := time.Parse("2006-01-02 15:04:05", typed); err == nil {
					row[key] = parsed
				}
			case float64:
				if parsed, ok := parseEventTime(typed); ok {
					row[key] = parsed
				}
			}
		}
	}
}

func isAnalyticsTimeColumn(key string) bool {
	switch key {
	case "timestamp", "received_at", "played_at", "scheduled_at", "connected_on", "connected_until", "date":
		return true
	}
	return strings.HasSuffix(key, "_at")
}
