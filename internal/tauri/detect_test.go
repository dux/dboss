package tauri

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeProbe answers the toolchain questions from fixed values.
type fakeProbe struct {
	goName string
	meta   cargoMetadata
}

func (p fakeProbe) goPackage(string, string) (string, error) { return p.goName, nil }

func (p fakeProbe) cargoMetadata(string, string) (cargoMetadata, error) { return p.meta, nil }

func useProbe(t *testing.T, p toolProbe) {
	t.Helper()
	previous := probe
	probe = p
	t.Cleanup(func() { probe = previous })
}

// oneBin is a single package in dir with the given targets, each "name:kind".
func oneBin(dir string, targets ...string) cargoMetadata {
	meta := cargoMetadata{TargetDirectory: filepath.Join(dir, "target")}
	meta.Packages = make([]struct {
		Name         string `json:"name"`
		ManifestPath string `json:"manifest_path"`
		Targets      []struct {
			Name string   `json:"name"`
			Kind []string `json:"kind"`
		} `json:"targets"`
	}, 1)
	meta.Packages[0].Name = "demo"
	meta.Packages[0].ManifestPath = filepath.Join(dir, "Cargo.toml")
	for _, target := range targets {
		name, kind, _ := strings.Cut(target, ":")
		meta.Packages[0].Targets = append(meta.Packages[0].Targets, struct {
			Name string   `json:"name"`
			Kind []string `json:"kind"`
		}{name, []string{kind}})
	}
	return meta
}

func touch(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestSidecarFromGoRun(t *testing.T) {
	dir := t.TempDir()
	target, err := DetectSidecar(dir, Commands{Box: "exec ./bin/server serve", Dev: "exec go run -tags dev ./app serve --verbose", Tauri: "exec ./bin/server serve"}, "/out/sidecar")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "build", "-trimpath", "-ldflags=-s -w", "-tags", "dev", "-o", "/out/sidecar", "./app"}
	if target.Toolchain != Go || !slices.Equal(target.Build, want) || target.Binary != "/out/sidecar" {
		t.Fatalf("target = %+v", target)
	}
	if !slices.Equal(target.Args, []string{"serve", "--verbose"}) {
		t.Fatalf("args = %q", target.Args)
	}
	if !strings.HasPrefix(target.Source, "command_dev:") {
		t.Fatalf("source = %q", target.Source)
	}
}

// A wrapper such as watchexec and a leading cd are looked through.
func TestSidecarFromWrappedCargoRun(t *testing.T) {
	dir := t.TempDir()
	useProbe(t, fakeProbe{meta: oneBin(filepath.Join(dir, "server"), "mailcog:bin", "mailcog:lib")})
	target, err := DetectSidecar(dir, Commands{Dev: "cd server && exec watchexec --restart --exts rs -- cargo run --features x -- start"}, "/out/sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if target.Toolchain != Cargo || target.Dir != filepath.Join(dir, "server") {
		t.Fatalf("target = %+v", target)
	}
	if want := []string{"cargo", "build", "--release", "--features", "x", "--bin", "mailcog"}; !slices.Equal(target.Build, want) {
		t.Fatalf("build = %q", target.Build)
	}
	if target.Binary != filepath.Join(dir, "server", "target", "release", "mailcog") || !slices.Equal(target.Args, []string{"start"}) {
		t.Fatalf("binary %q args %q", target.Binary, target.Args)
	}
}

// Two binaries and no --bin cannot be guessed.
func TestSidecarCargoNeedsOneBin(t *testing.T) {
	dir := t.TempDir()
	useProbe(t, fakeProbe{meta: oneBin(dir, "a:bin", "b:bin")})
	if _, err := DetectSidecar(dir, Commands{Dev: "cargo run"}, "/out/sidecar"); err == nil || !strings.Contains(err.Error(), "--bin") {
		t.Fatalf("expected a --bin hint, got %v", err)
	}
}

// command_tauri wins over every other view, and must name something buildable.
func TestSidecarExplicitCommandTauri(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "build", "desktop"), 0o755)
	target, err := DetectSidecar(dir, Commands{Box: "./bin/server", Dev: "go run ./app", Tauri: "exec ./build/desktop --local"}, "/out/sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if target.Toolchain != Binary || target.Binary != filepath.Join(dir, "build", "desktop") || len(target.Build) != 0 || !slices.Equal(target.Args, []string{"--local"}) {
		t.Fatalf("target = %+v", target)
	}
	if _, err := DetectSidecar(dir, Commands{Box: "./bin/server", Tauri: "bundle exec puma"}, "/out/sidecar"); err == nil || !strings.Contains(err.Error(), "command_tauri") {
		t.Fatalf("expected a command_tauri error, got %v", err)
	}
}

// Without a dev command, a go.mod with a root main package is built.
func TestSidecarFromGoMod(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "go.mod"), 0o644)
	useProbe(t, fakeProbe{goName: "main"})
	target, err := DetectSidecar(dir, Commands{Box: "bundle exec lux server", Tauri: "bundle exec lux server"}, "/out/sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if target.Toolchain != Go || target.Build[len(target.Build)-1] != "." || target.Source != "go.mod" {
		t.Fatalf("target = %+v", target)
	}
}

// The box command's program is used as it is when it is an executable inside the app, and never
// when it lives outside it.
func TestSidecarFromBoxBinary(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "bin", "server"), 0o755)
	target, err := DetectSidecar(dir, Commands{Box: "PORT_X=1 exec ./bin/server -e production", Tauri: "PORT_X=1 exec ./bin/server -e production"}, "/out/sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if target.Toolchain != Binary || !slices.Equal(target.Args, []string{"-e", "production"}) {
		t.Fatalf("target = %+v", target)
	}
	if _, err := DetectSidecar(dir, Commands{Box: "/bin/sh -c true", Tauri: "/bin/sh -c true"}, "/out/sidecar"); err == nil || !strings.Contains(err.Error(), "command_tauri") {
		t.Fatalf("a binary outside the app must be refused, got %v", err)
	}
}

func TestServerNeedsARustLibrary(t *testing.T) {
	dir := t.TempDir()
	if _, err := DetectServer(dir, Commands{Dev: "go run ./app"}); err == nil || !strings.Contains(err.Error(), Sidecar) {
		t.Fatalf("expected a rename-to-sidecar hint, got %v", err)
	}
	touch(t, filepath.Join(dir, "Cargo.toml"), 0o644)
	useProbe(t, fakeProbe{meta: oneBin(dir, "demo:bin")})
	if _, err := DetectServer(dir, Commands{Dev: "cargo run"}); err == nil || !strings.Contains(err.Error(), "serve(port: u16)") {
		t.Fatalf("expected a no-library error, got %v", err)
	}
	useProbe(t, fakeProbe{meta: oneBin(dir, "demo:bin", "demo:lib")})
	target, err := DetectServer(dir, Commands{Dev: "cargo run"})
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode != Server || target.Crate != "demo" || target.CrateDir != dir {
		t.Fatalf("target = %+v", target)
	}
}

func TestSplitWords(t *testing.T) {
	got := splitWords(`FOO="a b" exec ./x 'it''s' a\ b && cd web`)
	want := []string{"FOO=a b", "exec", "./x", "its", "a b", "&&", "cd", "web"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitWords = %q, want %q", got, want)
	}
}
