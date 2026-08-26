package main

import (
	"context"
)

// syncRadioPrograms rebuilds the TSDB `epg_programs_radio` table from the
// current Supabase radio EPG lineup. The radio reports/shows RPCs join this
// table for program names + images, and it previously held stale pre-migration
// Firebase/Google Storage image URLs. Idempotent: truncate + re-insert.
// Returns the number of programs synced.
func (s *server) syncRadioPrograms(ctx context.Context) (int, error) {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return 0, err
	}
	stations, programs, err := s.fetchEPG(ctx)
	if err != nil {
		return 0, err
	}

	// Collect radio program rows (Supabase epg_programs joined to radio stations).
	rows := make([]map[string]any, 0)
	for _, st := range stations {
		if st.LineupType != "radio" {
			continue
		}
		for _, p := range programs {
			if p.StationID != st.ID {
				continue
			}
			rows = append(rows, map[string]any{
				"program_name":    p.ProgramName,
				"presenter":       orEmptyStr(p.Presenter),
				"genre":           orEmptyStr(p.Genre),
				"start_time":      p.StartTime,
				"end_time":        p.EndTime,
				"days":            orEmptyStr(p.Days),
				"type":            orEmptyStr(p.Type),
				"image":           orEmptyStr(p.Image),
				"thumbnail":       orEmptyStr(p.Thumbnail),
				"image_landscape": nil,
			})
		}
	}

	if len(rows) == 0 {
		return 0, nil
	}

	// Replace the whole table (truncate then bulk insert). Use a direct insert
	// preserving the bigserial `id` ordering.
	if _, err := db.ExecContext(ctx, "TRUNCATE public.epg_programs_radio"); err != nil {
		return 0, err
	}

	// Order by start_time for a stable id order (matches prior table layout).
	// Inserting via multirow VALUES with the columns present.
	const cols = "(program_name, presenter, genre, start_time, end_time, days, type, image, thumbnail, image_landscape)"
	vals := "($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)"
	stmt := "INSERT INTO public.epg_programs_radio " + cols + " VALUES " + vals

	for _, r := range rows {
		args := []any{
			r["program_name"], r["presenter"], r["genre"],
			r["start_time"], r["end_time"], r["days"], r["type"],
			r["image"], r["thumbnail"], r["image_landscape"],
		}
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			return 0, err
		}
	}

	return len(rows), nil
}
