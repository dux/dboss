package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSandboxInheritsKeyByKey(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, ServerFileName)
	if err := os.WriteFile(host, []byte("defaults:\n  sandbox:\n    connect: [443]\n    sockets: /run/postgresql\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(host)
	if err != nil {
		t.Fatal(err)
	}
	if box := cfg.Defaults.Sandbox; !box.Enabled || !slices.Equal(box.Connect, Ports{443}) || !slices.Equal(box.Sockets, List{"/run/postgresql"}) {
		t.Fatalf("host block = %+v, want it on with its lists", box)
	}
	app, err := ParseApp([]byte("procfile:\n  web: ./server\n  worker: ./worker\nsandbox:\n  connect: [5432]\n  write: ./storage\nprocesses:\n  worker:\n    sandbox: false\n"), filepath.Join(dir, "apps", "shop", FileName), cfg.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	web := app.Process("web").Sandbox
	if !web.Enabled || !slices.Equal(web.Connect, Ports{5432}) || !slices.Equal(web.Write, List{"./storage"}) || !slices.Equal(web.Sockets, List{"/run/postgresql"}) {
		t.Fatalf("web sandbox = %+v: the app's lists replace the host's, the rest is inherited", web)
	}
	if app.Process("worker").Sandbox.Enabled {
		t.Fatal("processes.worker.sandbox: false must turn it off for that process")
	}
	off, err := ParseApp([]byte("procfile:\n  web: ./server\nsandbox:\n  enabled: false\n  bind: 25\n"), filepath.Join(dir, "apps", "shop", FileName), cfg.Defaults)
	if err != nil || off.Process("web").Sandbox.Enabled || !slices.Equal(off.Process("web").Sandbox.Bind, Ports{25}) {
		t.Fatalf("enabled: false in the block = %+v, %v", off.Process("web").Sandbox, err)
	}
}

func TestSandboxRejectsBadValues(t *testing.T) {
	for data, want := range map[string]string{
		"sandbox:\n  conect: [443]\n":    `unknown key "conect"`,
		"sandbox:\n  connect: [70000]\n": "port 70000 is outside 1-65535",
		"sandbox:\n  bind: http\n":       "must be a port or a list of ports",
		"sandbox:\n  read: [\"\"]\n":     "sandbox.read: has an empty path",
		"sandbox: maybe\n":               "must be true, false or a mapping",
	} {
		if _, err := ParseApp([]byte("procfile:\n  web: ./server\n"+data), "/srv/apps/demo/dboss.yaml", Default().Defaults); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error containing %q", data, err, want)
		}
	}
}
