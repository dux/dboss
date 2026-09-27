package logstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTrimWipesOldestTwoDaysUntilUnderLimit(t *testing.T) {
	store := New(t.TempDir(), time.Hour, nil, "", time.Hour, 0, 0)
	defer store.Close()
	ctx := context.Background()

	// Ten days of rows, one every 30 minutes, then the newest one.
	now := time.Now()
	oldest := now.Add(-10 * 24 * time.Hour)
	var entries []LogEntry
	for ts := oldest; ts.Before(now); ts = ts.Add(30 * time.Minute) {
		entries = append(entries, LogEntry{Time: ts, Source: "file", Process: "production.log", Level: "info", Message: "row", Raw: strings.Repeat("x", 2048)})
	}
	entries = append(entries, LogEntry{Time: now, Source: "file", Process: "production.log", Level: "info", Message: "newest", Raw: "newest"})
	if err := store.AppendLogs("demo", entries); err != nil {
		t.Fatal(err)
	}
	w, err := store.writer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`INSERT INTO exceptions (exp_uid, dump, first_at, last_at, count) VALUES ('boom', 'trace', 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`INSERT INTO audit (ts, actor, app, action, detail, result, error) VALUES (?, 'cli', 'demo', 'restart', '', 'ok', '')`, stamp(oldest)); err != nil {
		t.Fatal(err)
	}
	before, err := usedBytes(ctx, w.db)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Trim(ctx, "demo", 0); err != nil {
		t.Fatal(err)
	}
	if unchanged, _ := usedBytes(ctx, w.db); unchanged != before {
		t.Fatalf("limit 0 must not trim: %d -> %d", before, unchanged)
	}

	limit := before * 3 / 4
	if err := store.Trim(ctx, "demo", limit); err != nil {
		t.Fatal(err)
	}
	if used, err := usedBytes(ctx, w.db); err != nil || used > limit {
		t.Fatalf("used %d after trim, want <= %d (%v)", used, limit, err)
	}
	// A quarter over the limit takes two chunks of two days: the rows before day four go, the
	// rest stay.
	first, err := oldestRow(ctx, w.db)
	if err != nil || first.Before(oldest.Add(4*24*time.Hour)) || first.After(oldest.Add(4*24*time.Hour+time.Hour)) {
		t.Fatalf("oldest row after trim = %v, want four days after %v (%v)", first, oldest, err)
	}
	if newest, err := store.SearchLogs("demo", LogFilter{Query: "newest"}); err != nil || len(newest) != 1 {
		t.Fatalf("newest row must survive: %v %+v", err, newest)
	}
	var kept int
	if err := w.db.QueryRow(`SELECT (SELECT COUNT(*) FROM exceptions) + (SELECT COUNT(*) FROM audit)`).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("exceptions and audit must be kept: %v %d", err, kept)
	}
}

func TestTrimSkipsMissingDatabase(t *testing.T) {
	store := New(t.TempDir(), time.Hour, nil, "", time.Hour, 0, 0)
	defer store.Close()
	if err := store.Trim(context.Background(), "ghost", 1); err != nil {
		t.Fatal(err)
	}
	if exists, _ := store.exists("ghost"); exists {
		t.Fatal("trim must not create a database")
	}
}
