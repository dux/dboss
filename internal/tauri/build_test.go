package tauri

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
)

// render writes the whole shell project, and main.rs is the same bytes for every app and mode.
func TestRenderShellProject(t *testing.T) {
	cases := []struct {
		name   string
		server Target
	}{
		{"goapp", Target{Mode: Sidecar, Toolchain: Go, Args: []string{"serve"}}},
		{"rustapp", Target{Mode: Server, Toolchain: Cargo, Crate: "rust-app", CrateDir: "/src/rust app"}},
	}
	var mains []string
	for _, tc := range cases {
		project := t.TempDir()
		app := config.App{Procfile: map[string]config.ProcessSpec{tc.server.Mode: {Command: "x", Health: "/up"}}}
		app.Env = map[string]string{"GREETING": "hi"}
		b := builder{name: tc.name, project: project, app: app}
		// Icons are the tauri CLI's job; a matching stamp skips it.
		icon := filepath.Join(project, "icon.png")
		if err := writeIfChanged(icon, defaultIcon()); err != nil {
			t.Fatal(err)
		}
		b.opts.Icon = icon
		stampIcons(t, filepath.Join(project, "src-tauri"), icon)
		if err := b.render(tc.server); err != nil {
			t.Fatal(err)
		}
		tauriDir := filepath.Join(project, "src-tauri")
		main, err := os.ReadFile(filepath.Join(tauriDir, "src", "main.rs"))
		if err != nil {
			t.Fatal(err)
		}
		mains = append(mains, string(main))

		var conf map[string]any
		readJSON(t, filepath.Join(tauriDir, "tauri.conf.json"), &conf)
		bundle := conf["bundle"].(map[string]any)
		if conf["productName"] != tc.name || conf["identifier"] != "dev.dboss."+tc.name {
			t.Fatalf("%s: conf = %v", tc.name, conf)
		}
		_, hasSidecar := bundle["externalBin"]
		if hasSidecar != (tc.server.Mode == Sidecar) {
			t.Fatalf("%s: externalBin present = %v", tc.name, hasSidecar)
		}

		var shell shellConfig
		readJSON(t, filepath.Join(tauriDir, "shell.json"), &shell)
		if shell.App != tc.name || shell.Health != "/up" || shell.Env["GREETING"] != "hi" {
			t.Fatalf("%s: shell = %+v", tc.name, shell)
		}

		cargo, err := os.ReadFile(filepath.Join(tauriDir, "Cargo.toml"))
		if err != nil {
			t.Fatal(err)
		}
		linked := strings.Contains(string(cargo), `app_server = { package = "rust-app", path = "/src/rust app", optional = true }`)
		if linked != (tc.server.Mode == Server) {
			t.Fatalf("%s: Cargo.toml =\n%s", tc.name, cargo)
		}
		if _, err := os.Stat(filepath.Join(tauriDir, "capabilities")); err != nil {
			t.Fatalf("%s: capabilities folder: %v", tc.name, err)
		}
	}
	if mains[0] != mains[1] {
		t.Fatal("main.rs must not differ between apps or modes")
	}
}

func TestModeOf(t *testing.T) {
	app := func(names ...string) config.App {
		procfile := map[string]config.ProcessSpec{}
		for _, name := range names {
			procfile[name] = config.ProcessSpec{Command: "x"}
		}
		return config.App{Procfile: procfile}
	}
	if mode, err := modeOf(app(JS, Sidecar)); err != nil || mode != Sidecar {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if mode, err := modeOf(app(Server)); err != nil || mode != Server {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if _, err := modeOf(app(Sidecar, Server)); err == nil {
		t.Fatal("both entries must be refused")
	}
	if _, err := modeOf(app("web")); err == nil {
		t.Fatal("an app with neither entry must be refused")
	}
}

func TestIdentifierPart(t *testing.T) {
	for name, want := range map[string]string{"slack-clone": "slack-clone", "My_App 2": "my-app-2", "__": "app"} {
		if got := identifierPart(name); got != want {
			t.Fatalf("identifierPart(%q) = %q, want %q", name, got, want)
		}
	}
}

// stampIcons marks the icon set as generated from icon, so render skips the tauri CLI.
func stampIcons(t *testing.T, tauriDir, icon string) {
	t.Helper()
	data, err := os.ReadFile(icon)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIfChanged(filepath.Join(tauriDir, "icons", ".source"), iconStamp(data)); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
