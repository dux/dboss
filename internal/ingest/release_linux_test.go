package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/supervisor"
)

func TestTailReleasesIngestedHead(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "production.log")
	var content strings.Builder
	for i := 0; content.Len() < 3*releaseMin; i++ {
		fmt.Fprintf(&content, "line %d %s\n", i, strings.Repeat("x", 100))
	}
	writeLog(t, path, content.String())
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)
	module.runOnce()
	if module.unreleasable[path] {
		t.Skipf("filesystem of %s cannot collapse a range", dir)
	}

	at := sink.offsets[path]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= int64(content.Len()) || at.Base == 0 || at.Base+at.Offset != int64(content.Len()) || info.Size() != at.Offset {
		t.Fatalf("head not released: size %d of %d, offset %+v", info.Size(), content.Len(), at)
	}
	read := len(sink.entries)

	// The app keeps appending through its own handle; the next line lands after the kept tail.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("after release\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	quiet := time.Now().Add(-time.Minute)
	_ = os.Chtimes(path, quiet, quiet)
	module.runOnce()
	if len(sink.entries) != read+1 || sink.entries[read].Message != "after release" {
		t.Fatalf("line written after the release should be read once: %+v", sink.entries[read:])
	}
}

// A writer appending while passes cut the file must lose no line and repeat none: the cut and
// the appends are serialized by the inode lock.
func TestReleaseRacingWriterKeepsEveryLine(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "production.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	module := New(&fakeSealer{}, fakeApps{[]supervisor.Snapshot{snapshot(dir)}}, sink, nil, time.Second)

	const lines = 60000
	padding := strings.Repeat("x", 200)
	done := make(chan error, 1)
	go func() {
		for i := range lines {
			if _, err := fmt.Fprintf(file, "line %d %s\n", i, padding); err != nil {
				done <- err
				return
			}
		}
		done <- file.Close()
	}()
	for writing := true; writing; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			writing = false
		default:
			module.runOnce()
		}
	}
	quiet := time.Now().Add(-time.Minute)
	_ = os.Chtimes(path, quiet, quiet)
	module.runOnce()
	if module.unreleasable[path] {
		t.Skipf("filesystem of %s cannot collapse a range", dir)
	}
	if at := sink.offsets[path]; at.Base == 0 {
		t.Fatalf("the file was never cut: %+v", at)
	}

	seen := make([]int, lines)
	for _, entry := range sink.entries {
		var n int
		if _, err := fmt.Sscanf(entry.Message, "line %d ", &n); err != nil || n < 0 || n >= lines {
			t.Fatalf("unexpected row %q", entry.Message)
		}
		seen[n]++
	}
	for n, count := range seen {
		if count != 1 {
			t.Fatalf("line %d stored %d times", n, count)
		}
	}
}
