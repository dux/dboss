package logstore

import (
	"context"
	"database/sql"
	"time"

	"dboss/internal/logx"
)

// compactInterval is how often every database is trimmed to its max_db_size and vacuumed when it
// needs it, on top of the daily maintenance.
const compactInterval = 7 * time.Hour

// trimChunk is how much history one trim round wipes, counted from the oldest row.
const trimChunk = 48 * time.Hour

// Trim wipes the oldest two days of request, log and exception minute rows of one app's database
// once the space its rows use passes limit, and repeats from the new oldest row until it fits. An
// app that logs more than limit in two days is left with only what came after the cut.
// Exception summaries, audit rows, blocked counters and tail offsets are never trimmed. The freed
// pages are reused by later inserts; the daily VACUUM returns them to the filesystem once enough
// of the file is free.
func (s *Store) Trim(ctx context.Context, app string, limit int64) error {
	if limit <= 0 {
		return nil
	}
	w, err := s.writerForPrune(app)
	if err != nil || w == nil {
		return err
	}
	for {
		used, err := usedBytes(ctx, w.db)
		if err != nil || used <= limit {
			return err
		}
		oldest, err := oldestRow(ctx, w.db)
		if err != nil || oldest.IsZero() {
			return err
		}
		cutoff := oldest.Add(trimChunk)
		if _, err := w.db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, stamp(cutoff)); err != nil {
			return err
		}
		if _, err := w.db.ExecContext(ctx, `DELETE FROM logs WHERE ts < ?`, stamp(cutoff)); err != nil {
			return err
		}
		if _, err := w.db.ExecContext(ctx, `DELETE FROM exception_logs WHERE minute_at < ?`, cutoff.UnixMilli()); err != nil {
			return err
		}
	}
}

// usedBytes is the space the database's rows take: its pages minus the free list.
func usedBytes(ctx context.Context, db *sql.DB) (int64, error) {
	pages, free, size, err := pageStats(ctx, db)
	return (pages - free) * size, err
}

// pageStats reads the page count, the free-list page count and the page size.
func pageStats(ctx context.Context, db *sql.DB) (pages, free, size int64, err error) {
	err = db.QueryRowContext(ctx, `SELECT (SELECT page_count FROM pragma_page_count()), (SELECT freelist_count FROM pragma_freelist_count()), (SELECT page_size FROM pragma_page_size())`).Scan(&pages, &free, &size)
	return pages, free, size, err
}

// oldestRow is the earliest timestamp across the trimmed tables, zero when they are empty.
func oldestRow(ctx context.Context, db *sql.DB) (time.Time, error) {
	var oldest time.Time
	for _, query := range []string{`SELECT MIN(ts) FROM requests`, `SELECT MIN(ts) FROM logs`} {
		var ts sql.NullString
		if err := db.QueryRowContext(ctx, query).Scan(&ts); err != nil {
			return time.Time{}, err
		}
		if !ts.Valid {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, ts.String)
		if err != nil {
			return time.Time{}, err
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	var minute sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(minute_at) FROM exception_logs`).Scan(&minute); err != nil {
		return time.Time{}, err
	}
	if minute.Valid {
		if t := time.UnixMilli(minute.Int64); oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest, nil
}

// compactAll trims every database on disk to its max_db_size, then vacuums it when enough of it
// is free and truncates its WAL. Databases of removed apps and the host database take the host
// cap.
func (s *Store) compactAll() {
	known := map[string]bool{}
	if s.snapshotter != nil {
		for _, snapshot := range s.snapshotter.Snapshots() {
			known[snapshot.Name] = true
			s.compact(snapshot.Name, snapshot.MaxDBSize)
		}
	}
	for _, app := range s.diskApps() {
		if !known[app] {
			s.compact(app, s.hostMaxDBSize)
		}
	}
}

func (s *Store) compact(app string, limit int64) {
	if err := s.Trim(s.ctx, app, limit); err != nil {
		logx.Warnf("log trim %s: %v", app, err)
	}
	if err := s.Vacuum(s.ctx, app); err != nil {
		logx.Warnf("log vacuum %s: %v", app, err)
	}
}
