package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindServerInDirAndLive(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindServerInDir(dir); err == nil || !strings.Contains(err.Error(), ServerFileName) {
		t.Fatalf("empty folder = %v", err)
	}
	nested := filepath.Join(dir, ConfigDir)
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, filepath.Join(nested, ServerFileName), "apps: ./apps\n")
	if found, err := FindServerInDir(dir); err != nil || found != filepath.Join(nested, ServerFileName) {
		t.Fatalf("config/ fallback = %q, %v", found, err)
	}
	if _, err := FindInDir(dir); err == nil {
		t.Fatal("the app lookup found the server file")
	}

	base := filepath.Join(dir, ServerFileName)
	writeConfigFile(t, base, "apps: ./apps\n")
	if _, err := FindServerInDir(dir); err == nil || !strings.Contains(err.Error(), "keep one") {
		t.Fatalf("both places = %v", err)
	}
	if got := Live(base); got != base {
		t.Fatalf("Live without a local file = %q", got)
	}
	local := filepath.Join(dir, ServerLocalFileName)
	writeConfigFile(t, local, "apps: ./apps\n")
	if got := Live(base); got != local {
		t.Fatalf("Live = %q, want the server local file", got)
	}
}

func TestLocalFor(t *testing.T) {
	for path, want := range map[string]string{
		"/srv/dboss-server.yaml":       "/srv/dboss-server.local.yaml",
		"/srv/dboss-server.local.yaml": "/srv/dboss-server.local.yaml",
		"/srv/apps/a/dboss.yaml":       "/srv/apps/a/dboss.local.yaml",
		"/srv/apps/a/config/x.yaml":    "/srv/apps/a/config/dboss.local.yaml",
	} {
		if got := LocalFor(path); got != want {
			t.Errorf("LocalFor(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestFileNameMustMatchRole(t *testing.T) {
	for _, name := range []string{FileName, LocalFileName} {
		if _, err := Parse([]byte("apps: ./apps\n"), "/srv/"+name); err == nil || !strings.Contains(err.Error(), ServerFileName) {
			t.Errorf("host content in %s = %v", name, err)
		}
	}
	for _, name := range []string{ServerFileName, ServerLocalFileName} {
		if _, err := Parse([]byte("procfile:\n  web: ./server\n"), "/srv/"+name); err == nil || !strings.Contains(err.Error(), FileName) {
			t.Errorf("app content in %s = %v", name, err)
		}
	}
	// Any other name keeps the content-based role, so -c takes it.
	if cfg, err := Parse([]byte("apps: ./apps\n"), "/srv/host.yaml"); err != nil || cfg.App != nil {
		t.Fatalf("custom host name = %v", err)
	}
}
