// Package tauri packages a dboss app as a Tauri desktop app. The procfile names decide the shape:
// js is the one-shot frontend build, sidecar a compiled server in any language started beside a
// fixed Rust shell, and server a Rust library crate linked into that shell.
package tauri

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/template"

	"dboss/internal/config"
	"dboss/internal/fsutil"
)

//go:embed template
var templates embed.FS

// Options are the knobs of one build; everything else comes from the app's dboss.yaml.
type Options struct {
	// AppDir is the app folder, where dboss.yaml lives.
	AppDir string
	// Identifier is the bundle identifier; empty means dev.dboss.<app>.
	Identifier string
	// Out is where the finished bundles are copied; empty means <app>/dist/tauri.
	Out string
	// Bundles limits the bundle formats (app,dmg,deb,appimage,rpm); empty builds every format of the host.
	Bundles string
	// Icon is a square PNG; empty looks for icon.png, then public/icon.png, else a plain default.
	Icon string
	// Cache is the shared build folder; empty means <user cache dir>/dboss/tauri.
	Cache  string
	Stdout io.Writer
	Stderr io.Writer
}

// Window size of the shell's main window.
const (
	windowWidth  = 1200
	windowHeight = 800
)

// shellConfig is shell.json, the only app-specific input of main.rs.
type shellConfig struct {
	App    string            `json:"app"`
	Title  string            `json:"title"`
	Width  int               `json:"width"`
	Height int               `json:"height"`
	Health string            `json:"health"`
	Args   []string          `json:"args"`
	Env    map[string]string `json:"env"`
}

// Build runs the whole desktop build and returns the paths of the copied bundles.
func Build(opts Options) ([]string, error) {
	app, path, err := config.LoadProfile(opts.AppDir, config.TauriSuffix)
	if err != nil {
		return nil, err
	}
	appDir := config.BaseDir(path)
	name := filepath.Base(appDir)
	mode, err := modeOf(app)
	if err != nil {
		return nil, err
	}
	triple, err := preflight()
	if err != nil {
		return nil, err
	}
	cache := opts.Cache
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cache = filepath.Join(base, "dboss", "tauri")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	// One build at a time: every app shares the cargo target directory and its bundle folder.
	unlock, err := lock(filepath.Join(cache, "build.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	project := filepath.Join(cache, name+"-"+shortHash(appDir))
	b := builder{opts: opts, app: app, appDir: appDir, name: name, project: project, target: filepath.Join(cache, "target")}
	if js, ok := app.Procfile[JS]; ok {
		if err := b.runJS(js.Command); err != nil {
			return nil, err
		}
	}
	commands := commandsOf(opts.AppDir, mode, app)
	var server Target
	if mode == Sidecar {
		if server, err = DetectSidecar(appDir, commands, filepath.Join(project, "build", "sidecar")); err != nil {
			return nil, err
		}
		if err := b.buildSidecar(server, filepath.Join(project, "src-tauri", "binaries", "sidecar-"+triple)); err != nil {
			return nil, err
		}
	} else if server, err = DetectServer(appDir, commands); err != nil {
		return nil, err
	}
	b.logf("%s: %s (%s)", mode, server.Toolchain, server.Source)
	b.warnStatic()
	if err := b.render(server); err != nil {
		return nil, err
	}
	return b.bundle(mode)
}

// modeOf picks sidecar or server; an app declares exactly one.
func modeOf(app config.App) (string, error) {
	_, sidecar := app.Procfile[Sidecar]
	_, server := app.Procfile[Server]
	switch {
	case sidecar && server:
		return "", fmt.Errorf("procfile declares both %s and %s: keep %s for a binary in any language, %s for a Rust crate linked into the shell", Sidecar, Server, Sidecar, Server)
	case sidecar:
		return Sidecar, nil
	case server:
		return Server, nil
	}
	return "", fmt.Errorf("procfile needs a %s (a compiled server in any language) or a %s (a Rust library crate) entry for a desktop build", Sidecar, Server)
}

// commandsOf reads the server entry's command in all three views of the file. A view that fails
// to load simply has no command.
func commandsOf(dir, mode string, tauriApp config.App) Commands {
	commands := Commands{Tauri: tauriApp.Procfile[mode].Command}
	if app, _, err := config.LoadProfile(dir, ""); err == nil {
		commands.Box = app.Procfile[mode].Command
	}
	if app, _, err := config.LoadProfile(dir, config.DevSuffix); err == nil {
		commands.Dev = app.Procfile[mode].Command
	}
	return commands
}

// preflight checks the Rust toolchain and the tauri CLI, and returns the host target triple.
func preflight() (string, error) {
	if _, err := exec.LookPath("cargo"); err != nil {
		return "", errors.New("cargo not found: install Rust from https://rustup.rs")
	}
	if err := exec.Command("cargo", "tauri", "--version").Run(); err != nil {
		return "", errors.New("the tauri CLI is missing: cargo install tauri-cli --version '^2' --locked")
	}
	out, err := exec.Command("rustc", "-vV").Output()
	if err != nil {
		return "", commandError("rustc -vV", err)
	}
	for line := range strings.Lines(string(out)) {
		if triple, ok := strings.CutPrefix(line, "host: "); ok {
			return strings.TrimSpace(triple), nil
		}
	}
	return "", errors.New("rustc -vV names no host triple")
}

type builder struct {
	opts    Options
	app     config.App
	appDir  string
	name    string
	project string
	target  string
}

func (b builder) logf(format string, args ...any) {
	if b.opts.Stdout != nil {
		fmt.Fprintf(b.opts.Stdout, "tauri: "+format+"\n", args...)
	}
}

// run streams a command's output and fails with its name on a non-zero exit.
func (b builder) run(dir string, env []string, argv ...string) error {
	b.logf("$ %s", strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = b.opts.Stdout, b.opts.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// runJS runs the frontend build once, through the shell like every procfile command, with the
// app's config env.
func (b builder) runJS(command string) error {
	env := os.Environ()
	for _, key := range slices.Sorted(maps.Keys(b.app.Env)) {
		env = append(env, key+"="+b.app.Env[key])
	}
	env = append(env, "APP_NAME="+b.name, "PROC_TYPE="+JS)
	if err := b.run(b.appDir, env, "sh", "-c", command); err != nil {
		return fmt.Errorf("procfile.%s: %w", JS, err)
	}
	return nil
}

// buildSidecar builds the binary when the target has a build step and puts it at dest. An
// unchanged binary is not rewritten, so cargo keeps the shell it already linked.
func (b builder) buildSidecar(server Target, dest string) error {
	if len(server.Build) > 0 {
		if err := b.run(server.Dir, os.Environ(), server.Build...); err != nil {
			return err
		}
	}
	if server.Toolchain == Go {
		b.warnCgo(server)
	}
	if !executable(server.Binary) {
		return fmt.Errorf("%s is not an executable file: build it first, or put the build in js_tauri", server.Binary)
	}
	return copyFile(server.Binary, dest)
}

// warnCgo names the cgo packages a Go sidecar links: they tie the binary to C libraries the
// user's machine must have.
func (b builder) warnCgo(server Target) {
	pkg := server.Build[len(server.Build)-1]
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if .CgoFiles}}{{.ImportPath}}{{end}}", pkg)
	cmd.Dir = server.Dir
	out, err := cmd.Output()
	if err != nil {
		return
	}
	packages := strings.Fields(string(out))
	if len(packages) > 0 {
		b.logf("warning: the sidecar uses cgo (%s); the bundle runs only where its C libraries are installed", strings.Join(packages, ", "))
	}
}

// warnStatic flags a static folder: the proxy serves it on the box, but the desktop app has no
// proxy, so only what the server answers itself reaches the window.
func (b builder) warnStatic() {
	static := string(b.app.Web.Static)
	if static == "" {
		return
	}
	entries, err := os.ReadDir(resolve(b.appDir, static))
	if err != nil || len(entries) == 0 {
		return
	}
	b.logf("warning: %s is served by the dboss proxy on the box; the desktop app has no proxy, so the server must serve those files itself", static)
}

// render writes the shell project: the fixed Rust files, Cargo.toml, tauri.conf.json, shell.json
// and the icons.
func (b builder) render(server Target) error {
	err := fs.WalkDir(templates, "template", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := templates.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "template/")
		if name, ok := strings.CutSuffix(rel, ".tmpl"); ok {
			rel = name
			if data, err = renderTemplate(path, data, server); err != nil {
				return err
			}
		}
		return writeIfChanged(filepath.Join(b.project, rel), data)
	})
	if err != nil {
		return err
	}
	tauriDir := filepath.Join(b.project, "src-tauri")
	// Empty on purpose: the window shows the server's own pages, which get no IPC. tauri-build
	// watches the folder, and a missing one marks the shell stale on every build.
	if err := os.MkdirAll(filepath.Join(tauriDir, "capabilities"), 0o755); err != nil {
		return err
	}
	shell := shellConfig{App: b.name, Title: b.name, Width: windowWidth, Height: windowHeight, Health: b.app.Procfile[server.Mode].Health, Args: server.Args, Env: b.app.Env}
	if shell.Args == nil {
		shell.Args = []string{}
	}
	if shell.Env == nil {
		shell.Env = map[string]string{}
	}
	if err := writeJSON(filepath.Join(tauriDir, "shell.json"), shell); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(tauriDir, "tauri.conf.json"), b.conf(server)); err != nil {
		return err
	}
	return b.icons(tauriDir)
}

func renderTemplate(name string, data []byte, server Target) ([]byte, error) {
	tmpl, err := template.New(name).Parse(string(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, server); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// conf is tauri.conf.json. The window is created by main.rs, so the config declares none.
func (b builder) conf(server Target) map[string]any {
	identifier := b.opts.Identifier
	if identifier == "" {
		identifier = "dev.dboss." + identifierPart(b.name)
	}
	bundle := map[string]any{
		"active":    true,
		"targets":   "all",
		"icon":      []string{"icons/32x32.png", "icons/128x128.png", "icons/128x128@2x.png", "icons/icon.icns", "icons/icon.ico"},
		"resources": []string{"shell.json"},
	}
	if server.Mode == Sidecar {
		bundle["externalBin"] = []string{"binaries/sidecar"}
	}
	return map[string]any{
		"$schema":        "https://schema.tauri.app/config/2",
		"productName":    b.name,
		"mainBinaryName": b.name,
		"version":        "0.1.0",
		"identifier":     identifier,
		"build":          map[string]any{"frontendDist": "../dist"},
		"app":            map[string]any{"windows": []any{}, "security": map[string]any{"csp": nil}},
		"bundle":         bundle,
	}
}

// icons generates the icon set from the app's PNG, or from a plain default, once per source image.
func (b builder) icons(tauriDir string) error {
	source := b.opts.Icon
	if source == "" {
		for _, candidate := range []string{"icon.png", "public/icon.png"} {
			if exists(filepath.Join(b.appDir, candidate)) {
				source = filepath.Join(b.appDir, candidate)
				break
			}
		}
	}
	if source == "" {
		source = filepath.Join(b.project, "icon.png")
		if err := writeIfChanged(source, defaultIcon()); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	icons := filepath.Join(tauriDir, "icons")
	marker := filepath.Join(icons, ".source")
	stamp := iconStamp(data)
	if current, err := os.ReadFile(marker); err == nil && bytes.Equal(current, stamp) {
		return nil
	}
	b.logf("icons from %s", source)
	if out, err := exec.Command("cargo", "tauri", "icon", source, "--output", icons).CombinedOutput(); err != nil {
		return fmt.Errorf("cargo tauri icon %s: %w\n%s", source, err, out)
	}
	return fsutil.WriteFile(marker, stamp, 0o644)
}

// iconStamp identifies the source image an icon set was generated from.
func iconStamp(data []byte) []byte {
	sum := sha256.Sum256(data)
	return []byte(hex.EncodeToString(sum[:]) + "\n")
}

// bundle runs cargo tauri build against the shared target directory and copies the bundles out.
func (b builder) bundle(mode string) ([]string, error) {
	bundleDir := filepath.Join(b.target, "release", "bundle")
	if err := os.RemoveAll(bundleDir); err != nil {
		return nil, err
	}
	argv := []string{"cargo", "tauri", "build", "--ci"}
	if mode == Server {
		argv = append(argv, "--features", "server")
	}
	if b.opts.Bundles != "" {
		argv = append(argv, "--bundles", b.opts.Bundles)
	}
	// The build dir is pinned too: a per-workspace build-dir from the user's cargo config would
	// compile tauri again for every app.
	env := append(os.Environ(), "CARGO_TARGET_DIR="+b.target, "CARGO_BUILD_BUILD_DIR="+b.target)
	if err := b.run(b.project, env, argv...); err != nil {
		return nil, err
	}
	out := b.opts.Out
	if out == "" {
		out = filepath.Join(b.appDir, "dist", "tauri")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	kinds, err := os.ReadDir(bundleDir)
	if err != nil {
		return nil, err
	}
	copied := []string{}
	for _, kind := range kinds {
		items, err := os.ReadDir(filepath.Join(bundleDir, kind.Name()))
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !bundleItem.MatchString(item.Name()) {
				continue
			}
			dest := filepath.Join(out, item.Name())
			if err := os.RemoveAll(dest); err != nil {
				return nil, err
			}
			if err := copyTree(filepath.Join(bundleDir, kind.Name(), item.Name()), dest); err != nil {
				return nil, err
			}
			copied = append(copied, dest)
		}
	}
	sort.Strings(copied)
	return copied, nil
}

// bundleItem matches the finished bundles, not the bundler's scratch files beside them.
var bundleItem = regexp.MustCompile(`\.(app|dmg|deb|rpm|AppImage|msi|exe)$`)

// defaultIcon is a plain rounded-off square, so an app without an icon still bundles.
func defaultIcon() []byte {
	const size = 1024
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	fill := color.RGBA{R: 0x20, G: 0x6b, B: 0xc4, A: 0xff}
	const radius = 180
	for y := range size {
		for x := range size {
			dx, dy := max(radius-x, x-(size-1-radius), 0), max(radius-y, y-(size-1-radius), 0)
			if dx*dx+dy*dy <= radius*radius {
				img.Set(x, y, fill)
			}
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}

// identifierPart turns an app name into a bundle identifier segment: lowercase letters, digits
// and hyphens.
func identifierPart(name string) string {
	part := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	if part == "" {
		return "app"
	}
	return part
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:4])
}

// lock takes an exclusive flock on path until the returned func runs.
func lock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// writeIfChanged leaves an identical file alone, so cargo does not rebuild the shell for nothing.
func writeIfChanged(path string, data []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return nil
	}
	return fsutil.WriteFile(path, data, 0o644)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeIfChanged(path, append(data, '\n'))
}

// copyFile copies src with its mode, leaving an identical dest untouched.
func copyFile(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if current, err := os.Stat(dest); err == nil && current.Mode() == info.Mode() {
		if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, data) {
			return nil
		}
	}
	return fsutil.WriteFile(dest, data, info.Mode().Perm())
}

// copyTree copies a file or a bundle folder, keeping modes and symlinks.
func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyFile(path, target)
	})
}
