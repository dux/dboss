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
