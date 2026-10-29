package console

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/fsutil"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/sysinfo"
)

// pgHandler is a console whose PostgreSQL service is real but has no server, with one recorded
// dump on disk: downloading it never touches a server.
func pgHandler(t *testing.T, archive []byte) (*Handler, pg.Backup) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Apps = "/apps"
	cfg.Dir = dir
	cfg.StateDir = filepath.Join(dir, ".dboss")
	cfg.Management.Host = config.List{"dboss.lvh.me", "dboss.internal"}
	cfg.Management.Admins = []string{"admin@example.com"}
	cfg.Postgres.Enabled = true

	path := filepath.Join(dir, "BACKUP_2026-09-21T04-00-00Z.zip")
	if err := os.WriteFile(path, archive, 0o640); err != nil {
		t.Fatal(err)
	}
	entry := pg.Backup{ID: filepath.Base(path), Database: "app", Status: "ok", LocalPath: path}
	if err := fsutil.WriteJSON(filepath.Join(cfg.StateDir, "pg-backups.json"), []pg.Backup{entry}, 0o640); err != nil {
		t.Fatal(err)
	}

	service := ops.New(&fakeManager{}, fakeLogs{}, pg.New(cfg, nil), nil, nil, nil)
	handler, err := New(cfg, authcog.NewWithKey([]byte("01234567890123456789012345678901")), service, newFakeStore(), &fakeSys{snapshot: sysinfo.Snapshot{Host: sysinfo.Host{Hostname: "box"}}})
	if err != nil {
		t.Fatal(err)
	}
	return handler, entry
}

func TestPGBackupDownloadServesTheArchive(t *testing.T) {
	archive := []byte("PK\x03\x04 dump")
	handler, entry := pgHandler(t, archive)
	cookie, session := sessionCookie(t, handler)

	download := call(t, handler, cookie, session, http.MethodGet, "/ui/pg/backup/download?id="+entry.ID, "")
	if download.Code != http.StatusOK {
		t.Fatalf("download should serve the archive: %d %s", download.Code, download.Body.String())
	}
	if !bytes.Equal(download.Body.Bytes(), archive) {
		t.Fatal("download should return the archive byte for byte")
	}
	if got := download.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, entry.ID) {
		t.Fatalf("download should be an attachment named after the dump, got %q", got)
	}

	missing := call(t, handler, cookie, session, http.MethodGet, "/ui/pg/backup/download?id=nope", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("an unknown id should be a 404: %d", missing.Code)
	}
}
