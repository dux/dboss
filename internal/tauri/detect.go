package tauri

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The procfile names a desktop build reads. An app declares exactly one of Sidecar and Server.
const (
	// JS is the one-shot frontend build, normally declared as js_tauri.
	JS = "js"
	// Sidecar is the main server in any compiled language, started by the shell as its own process.
	Sidecar = "sidecar"
	// Server is a Rust library crate linked into the shell, so the bundle holds one binary.
	Server = "server"
)

// Toolchains a sidecar is built with; Binary means the file is used as it is.
const (
	Go     = "go"
	Cargo  = "cargo"
	Binary = "binary"
)

// Target is the resolved server of a desktop build.
type Target struct {
	Mode      string
	Toolchain string
	// Build is the command that produces Binary, run in Dir; empty when Binary is used as it is.
	Build []string
	Dir   string
	// Binary is the sidecar executable, absolute.
	Binary string
	// Args are passed to the sidecar at launch.
	Args []string
	// Crate is the server mode's Cargo package and CrateDir its folder.
	Crate    string
	CrateDir string
	// Source says which command or file the target was read from, for the build log.
	Source string
}

// Commands are the three views of the server entry's command: the box (no suffix), the dev
// session (_dev) and the desktop build (_tauri). Empty means the view has no such entry.
type Commands struct {
	Box, Dev, Tauri string
}

// probe answers the questions detection asks a toolchain; a var so tests need neither Go nor Cargo.
var probe toolProbe = execProbe{}

type toolProbe interface {
	goPackage(dir, pkg string) (string, error)
	cargoMetadata(dir, manifest string) (cargoMetadata, error)
}

type cargoMetadata struct {
	Packages []struct {
		Name         string `json:"name"`
		ManifestPath string `json:"manifest_path"`
		Targets      []struct {
			Name string   `json:"name"`
			Kind []string `json:"kind"`
		} `json:"targets"`
	} `json:"packages"`
	TargetDirectory string `json:"target_directory"`
}

type execProbe struct{}

func (execProbe) goPackage(dir, pkg string) (string, error) {
	cmd := exec.Command("go", "list", "-f", "{{.Name}}", pkg)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (execProbe) cargoMetadata(dir, manifest string) (cargoMetadata, error) {
	args := []string{"metadata", "--no-deps", "--format-version", "1"}
	if manifest != "" {
		args = append(args, "--manifest-path", manifest)
	}
	cmd := exec.Command("cargo", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return cargoMetadata{}, commandError("cargo metadata", err)
	}
	var meta cargoMetadata
	return meta, json.Unmarshal(out, &meta)
}

// errNoMatch is a candidate that names no toolchain; detection moves on to the next one.
var errNoMatch = errors.New("no match")

// DetectSidecar resolves how the sidecar binary is produced, first match wins: an explicit
// command_tauri, a `go run` or `cargo run` in the dev command, a go.mod or Cargo.toml in the app
// folder, then the box command's program when it is an executable inside the app. A Go build
// writes its binary to out; Cargo keeps its own target directory.
func DetectSidecar(appDir string, commands Commands, out string) (Target, error) {
	explicit := commands.Tauri != "" && commands.Tauri != commands.Box
	if explicit {
		target, err := fromCommand(appDir, commands.Tauri, out, true)
		if errors.Is(err, errNoMatch) {
			return Target{}, fmt.Errorf("procfile.%s.command_tauri %q names no binary, go run or cargo run", Sidecar, commands.Tauri)
		}
		return withSource(target, err, "command_tauri")
	}
	if commands.Dev != "" {
		target, err := fromCommand(appDir, commands.Dev, out, false)
		if !errors.Is(err, errNoMatch) {
			return withSource(target, err, "command_dev")
		}
	}
	target, err := fromMarkers(appDir, out)
	if !errors.Is(err, errNoMatch) {
		return target, err
	}
	target, err = fromCommand(appDir, commands.Tauri, out, true)
	if !errors.Is(err, errNoMatch) {
		return withSource(target, err, "command")
	}
	return Target{}, fmt.Errorf("cannot tell how to build the %s from %q: set procfile.%s.command_tauri to a compiled binary, e.g. command_tauri: ./bin/server", Sidecar, commands.Tauri, Sidecar)
}

func withSource(target Target, err error, key string) (Target, error) {
	if err == nil {
		target.Source = key + ": " + target.Source
	}
	return target, err
}

// fromCommand reads a toolchain out of one shell command: `go run`, `cargo run`, or, when
// binaryOK, a program that is an executable file inside the app folder. A leading `cd dir &&`
// moves the working folder, and `exec`, env assignments and wrappers such as watchexec are skipped.
func fromCommand(appDir, command, out string, binaryOK bool) (Target, error) {
	dir := appDir
	for _, segment := range segments(splitWords(command)) {
		if len(segment) >= 2 && segment[0] == "cd" {
			dir = resolve(dir, segment[1])
			continue
		}
		for i := 0; i+1 < len(segment); i++ {
			switch {
			case filepath.Base(segment[i]) == "go" && segment[i+1] == "run":
				return goRun(dir, segment[i+2:], command, out)
			case filepath.Base(segment[i]) == "cargo" && segment[i+1] == "run":
				return cargoRun(dir, segment[i+2:], command)
			}
		}
		if !binaryOK {
			continue
		}
		program, args := programOf(segment)
		if program == "" {
			continue
		}
		path := resolve(dir, program)
		if !strings.ContainsRune(program, '/') || !inside(appDir, path) || !executable(path) {
			continue
		}
		return Target{Mode: Sidecar, Toolchain: Binary, Dir: dir, Binary: path, Args: args, Source: command}, nil
	}
	return Target{}, errNoMatch
}

// goValueFlags are the go build flags that take the next word as their value.
var goValueFlags = []string{"-C", "-p", "-tags", "-ldflags", "-gcflags", "-asmflags", "-mod", "-modfile", "-overlay", "-pkgdir", "-toolexec", "-exec"}

// goRun turns `go run [flags] <pkg> [args]` into a go build of the same package. -exec is a run
// flag only, so it is dropped.
func goRun(dir string, words []string, source, out string) (Target, error) {
	flags := []string{}
	pkg := ""
	i := 0
	for ; i < len(words); i++ {
		word := words[i]
		if !strings.HasPrefix(word, "-") {
			pkg = word
			i++
			break
		}
		takesValue := slices.Contains(goValueFlags, word)
		if word == "-exec" {
			i++
			continue
		}
		flags = append(flags, word)
		if takesValue && i+1 < len(words) {
			i++
			flags = append(flags, words[i])
		}
	}
	if pkg == "" {
		return Target{}, fmt.Errorf("%q: go run names no package", source)
	}
	build := []string{"go", "build"}
	if !slices.ContainsFunc(flags, func(flag string) bool { return strings.HasPrefix(flag, "-trimpath") }) {
		build = append(build, "-trimpath")
	}
	if !slices.ContainsFunc(flags, func(flag string) bool { return strings.HasPrefix(flag, "-ldflags") }) {
		build = append(build, "-ldflags=-s -w")
	}
	build = append(build, flags...)
	build = append(build, "-o", out, pkg)
	return Target{Mode: Sidecar, Toolchain: Go, Build: build, Dir: dir, Binary: out, Args: words[i:], Source: source}, nil
}

// cargoValueFlags are the cargo build flags that take the next word as their value.
var cargoValueFlags = []string{"--bin", "-p", "--package", "--features", "-F", "--manifest-path", "--target", "--profile", "--config", "-Z"}

// cargoRun turns `cargo run [flags] [-- args]` into a release build of the same binary.
func cargoRun(dir string, words []string, source string) (Target, error) {
	flags := []string{}
	args := []string{}
	bin, pkg, manifest := "", "", ""
	for i := 0; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			args = words[i+1:]
			break
		}
		if word == "--release" || word == "-r" {
			continue
		}
		flags = append(flags, word)
		if !slices.Contains(cargoValueFlags, word) || i+1 >= len(words) {
			continue
		}
		i++
		flags = append(flags, words[i])
		switch word {
		case "--bin":
			bin = words[i]
		case "-p", "--package":
			pkg = words[i]
		case "--manifest-path":
			manifest = resolve(dir, words[i])
		}
	}
	meta, err := probe.cargoMetadata(dir, manifest)
	if err != nil {
		return Target{}, err
	}
	if bin == "" {
		if bin, err = onlyBin(meta, dir, pkg, manifest); err != nil {
			return Target{}, err
		}
	}
	build := append([]string{"cargo", "build", "--release"}, flags...)
	if !slices.Contains(flags, "--bin") {
		build = append(build, "--bin", bin)
	}
	binary := filepath.Join(meta.TargetDirectory, "release", bin)
	return Target{Mode: Sidecar, Toolchain: Cargo, Build: build, Dir: dir, Binary: binary, Args: args, Source: source}, nil
}

// onlyBin is the one bin target of the selected package: -p, else the manifest in dir, else the
// only package.
func onlyBin(meta cargoMetadata, dir, pkg, manifest string) (string, error) {
	if manifest == "" {
		manifest = filepath.Join(dir, "Cargo.toml")
	}
	for _, candidate := range meta.Packages {
		if pkg != "" && candidate.Name != pkg || pkg == "" && len(meta.Packages) > 1 && !samePath(candidate.ManifestPath, manifest) {
			continue
		}
		bins := []string{}
		for _, target := range candidate.Targets {
			if slices.Contains(target.Kind, "bin") {
				bins = append(bins, target.Name)
			}
		}
		if len(bins) != 1 {
			return "", fmt.Errorf("package %s has %d binaries (%s): pick one with cargo run --bin <name> in command_dev, or set command_tauri", candidate.Name, len(bins), strings.Join(bins, ", "))
		}
		return bins[0], nil
	}
	return "", fmt.Errorf("no cargo package to build in %s: name one with -p", dir)
}

// fromMarkers is the fallback when no command names a toolchain: a go.mod whose root package is a
// main package, or a Cargo.toml with exactly one binary.
func fromMarkers(dir, out string) (Target, error) {
	if exists(filepath.Join(dir, "go.mod")) {
		if name, err := probe.goPackage(dir, "."); err == nil && name == "main" {
			return Target{Mode: Sidecar, Toolchain: Go, Build: []string{"go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "."}, Dir: dir, Binary: out, Source: "go.mod"}, nil
		}
	}
	if exists(filepath.Join(dir, "Cargo.toml")) {
		return cargoRun(dir, nil, "Cargo.toml")
	}
	return Target{}, errNoMatch
}

// DetectServer resolves the server mode's crate: the Cargo package in the app folder (or where
// the dev command's `cd` or --manifest-path points), which must have a library target.
func DetectServer(appDir string, commands Commands) (Target, error) {
	dir, manifest, pkg := cargoRunSelection(appDir, commands.Tauri)
	if dir == "" {
		dir, manifest, pkg = cargoRunSelection(appDir, commands.Dev)
	}
	if dir == "" {
		dir = appDir
	}
	if manifest == "" {
		manifest = filepath.Join(dir, "Cargo.toml")
	}
	if !exists(manifest) {
		return Target{}, fmt.Errorf("procfile.%s must be a Rust crate and %s is missing: rename the entry to %s to wrap a binary in any language", Server, manifest, Sidecar)
	}
	meta, err := probe.cargoMetadata(dir, manifest)
	if err != nil {
		return Target{}, err
	}
	for _, candidate := range meta.Packages {
		if pkg != "" && candidate.Name != pkg || pkg == "" && len(meta.Packages) > 1 && !samePath(candidate.ManifestPath, manifest) {
			continue
		}
		for _, target := range candidate.Targets {
			if slices.Contains(target.Kind, "lib") || slices.Contains(target.Kind, "rlib") {
				crateDir := filepath.Dir(candidate.ManifestPath)
				return Target{Mode: Server, Toolchain: Cargo, Crate: candidate.Name, CrateDir: crateDir, Dir: crateDir, Source: candidate.ManifestPath}, nil
			}
		}
		return Target{}, fmt.Errorf("crate %s has no library target: add src/lib.rs with `pub async fn serve(port: u16)`, or rename procfile.%s to %s", candidate.Name, Server, Sidecar)
	}
	return Target{}, fmt.Errorf("no cargo package in %s", manifest)
}

// cargoRunSelection is the folder, manifest and package of the first `cargo run` in command, or
// an empty dir when it has none.
func cargoRunSelection(appDir, command string) (string, string, string) {
	dir := appDir
	for _, segment := range segments(splitWords(command)) {
		if len(segment) >= 2 && segment[0] == "cd" {
			dir = resolve(dir, segment[1])
			continue
		}
		for i := 0; i+1 < len(segment); i++ {
			if filepath.Base(segment[i]) != "cargo" || segment[i+1] != "run" {
				continue
			}
			manifest, pkg := "", ""
			for j := i + 2; j+1 < len(segment) && segment[j] != "--"; j++ {
				switch segment[j] {
				case "--manifest-path":
					manifest = resolve(dir, segment[j+1])
				case "-p", "--package":
					pkg = segment[j+1]
				}
			}
			return dir, manifest, pkg
		}
	}
	return "", "", ""
}

// envAssign matches a leading NAME=value word of a shell command.
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// programOf is the program a segment runs and its args, past `exec` and env assignments.
func programOf(segment []string) (string, []string) {
	for i, word := range segment {
		if word == "exec" || envAssign.MatchString(word) {
			continue
		}
		return word, segment[i+1:]
	}
	return "", nil
}

// splitWords splits a shell command into words, honoring quotes and backslashes; the operators
// &&, ||, ; and | come back as their own words when written apart.
func splitWords(command string) []string {
	words := []string{}
	var word strings.Builder
	inWord := false
	quote := rune(0)
	escaped := false
	for _, r := range command {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// segments splits words at the shell operators, so `cd web && bun run build` is two commands.
func segments(words []string) [][]string {
	result := [][]string{}
	current := []string{}
	for _, word := range words {
		if word == "&&" || word == "||" || word == ";" || word == "|" {
			result = append(result, current)
			current = []string{}
			continue
		}
		current = append(current, word)
	}
	return append(result, current)
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// inside reports whether path is dir or below it, so a sidecar never ships a system binary.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// commandError adds a failed command's stderr to its error.
func commandError(name string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", name, err)
}
