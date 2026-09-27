package logstore

import (
	"context"
	"database/sql"
	"time"

	"dboss/internal/logx"

	_ "modernc.org/sqlite"
)

// Prune deletes rows older than their retention from one app's database. Request rows and app
// log files use retention, process stdout and the dboss daemon log use stdoutRetention. A zero
// retention disables the whole store, matching Request/RecordLogs.
func (s *Store) Prune(ctx context.Context, app string, retention, stdoutRetention time.Duration) error {
	w, err := s.writerForPrune(app)
	if err != nil {
		return err
	}
	if w == nil {
		return nil
	}
	// Audit rows live in the host database and use their own retention, independent of the log
	// retention that may be disabled for the app.
	if app == HostApp && s.auditRetention > 0 {
		if _, err := w.db.ExecContext(ctx, `DELETE FROM audit WHERE ts < ?`, stamp(time.Now().Add(-s.auditRetention))); err != nil {
			return err
		}
	}
	if retention <= 0 {
		return nil
	}
	if _, err := w.db.ExecContext(ctx, `DELETE FROM requests WHERE ts < ?`, stamp(time.Now().Add(-retention))); err != nil {
		return err
	}
	if _, err := w.db.ExecContext(ctx, `DELETE FROM logs WHERE source = 'file' AND ts < ?`, stamp(time.Now().Add(-retention))); err != nil {
		return err
	}
	// Exception summaries and dumps live in `exceptions` and are kept; only the per-minute
	// occurrence rows age out.
	if _, err := w.db.ExecContext(ctx, `DELETE FROM exception_logs WHERE minute_at < ?`, time.Now().Add(-retention).UnixMilli()); err != nil {
		return err
	}
	if stdoutRetention > 0 {
		cutoff := stamp(time.Now().Add(-stdoutRetention))
		// Everything that is not an app log file is the short-lived console stream, including
		// legacy rows written before the channels existed.
		if _, err := w.db.ExecContext(ctx, `DELETE FROM logs WHERE source <> 'file' AND ts < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// pruneAll applies retention to every database on disk once.
func (s *Store) pruneAll() {
	known := map[string]bool{}
	for _, snapshot := range s.snapshotter.Snapshots() {
		known[snapshot.Name] = true
		if err := s.Prune(s.ctx, snapshot.Name, snapshot.LogRetention, snapshot.StdoutRetention); err != nil {
			logx.Warnf("log prune %s: %v", snapshot.Name, err)
		}
	}
	// Databases left behind by apps removed from the config keep the host retention so they
	// cannot grow forever after removal.
	for _, app := range s.diskApps() {
		if known[app] || app == HostApp {
			continue
		}
		if err := s.Prune(s.ctx, app, s.hostRetention, s.hostRetention); err != nil {
			logx.Warnf("log prune %s: %v", app, err)
		}
	}
	if err := s.Prune(s.ctx, HostApp, s.hostRetention, s.hostRetention); err != nil {
		logx.Warnf("log prune %s: %v", HostApp, err)
	}
}

// VACUUM copies the whole database through the WAL, so it only runs when the free pages are worth
// it: at least vacuumMinFree bytes and a quarter of the file.
const (
	vacuumMinFree  = 16 << 20
	vacuumMinShare = 4
)

// Vacuum rewrites one app's database when enough of it is free pages, then truncates the WAL the
// rewrite went through. A missing database is a no-op.
func (s *Store) Vacuum(ctx context.Context, app string) error {
	db, done, err := s.maintenanceDB(app)
	if err != nil || db == nil {
		return err
	}
	defer done()
	pages, free, size, err := pageStats(ctx, db)
	if err != nil {
		return err
	}
	if free*size >= vacuumMinFree && free*vacuumMinShare >= pages {
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			return err
		}
	}
	return truncateWAL(ctx, db)
}

// truncateWAL checkpoints every frame and cuts the WAL to zero. A reader holding an old snapshot
// makes SQLite skip the truncate; the next pass tries again.
func truncateWAL(ctx context.Context, db *sql.DB) error {
	var busy, log, checkpointed int64
	return db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed)
}

// maintenanceDB is the app's writer connection, or one opened for this pass when no writer holds
// the database, so maintenance never creates a database or a writer loop. A missing database is
// nil.
func (s *Store) maintenanceDB(app string) (*sql.DB, func(), error) {
	s.mu.Lock()
	w := s.apps[app]
	s.mu.Unlock()
	if w != nil {
		return w.db, func() {}, nil
	}
	if exists, err := s.exists(app); !exists {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", s.dbPath(app))
	if err != nil {
		return nil, nil, err
	}
	return db, func() { _ = db.Close() }, nil
}
