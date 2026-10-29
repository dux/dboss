package pg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
)

func testService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	return New(config.Config{Dir: dir, StateDir: filepath.Join(dir, ".dboss")}, nil)
}

func TestBackupFileServesOnlyACompletedDumpOnDisk(t *testing.T) {
	service := testService(t)
	path := filepath.Join(t.TempDir(), "BACKUP.zip")
	if err := os.WriteFile(path, []byte("zip"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []Backup{
		{ID: "ok", Database: "app", Status: "ok", LocalPath: path},
		{ID: "failed", Database: "app", Status: "failed"},
		{ID: "gone", Database: "app", Status: "ok", LocalPath: path + ".missing"},
	} {
		if err := service.catalog.record(entry); err != nil {
			t.Fatal(err)
		}
	}

	found, got, err := service.BackupFile("ok")
	if err != nil || got != path || found.ID != "ok" {
		t.Fatalf("BackupFile(ok) = %+v %q %v", found, got, err)
	}
	for _, id := range []string{"nope", "failed", "gone"} {
		if _, _, err := service.BackupFile(id); err == nil {
			t.Fatalf("BackupFile(%s) should fail", id)
		}
	}
}

func TestUniqueDumpKeepsIDsApartWithinOneSecond(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)

	first, firstPath := uniqueDump(dir, at)
	if err := os.WriteFile(firstPath, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	second, secondPath := uniqueDump(dir, at)
	if second == first || secondPath == firstPath {
		t.Fatalf("a second dump in the same second needs its own name, got %q twice", first)
	}
	if !strings.HasSuffix(second, "_2.zip") {
		t.Fatalf("the suffix should follow the stamp, got %q", second)
	}
}
