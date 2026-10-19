package apps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/schedule"
)

type Command struct {
	Name string   `json:"name"`
	Line string   `json:"line"`
	Argv []string `json:"argv,omitempty"` // dboss exec only
}

type App struct {
	Name     string             `json:"name"`
	Dir      string             `json:"dir"`
	Commands map[string]Command `json:"commands"`
	Cron     map[string]CronJob `json:"cron"`
	Hooks    map[string]Hook    `json:"hooks"`
	// Lifecycle holds the create, start and destroy steps that are set, plus dboss's own branch
	// step when the app sets `branch` and its folder is a checkout of its own.
	Lifecycle map[string]Step `json:"lifecycle"`
	// Env is the daemon environment plus mise; FileEnv is .env overlaid by .env.local. Config
	// env sits between them at process start, so it is applied when the process env is built.
	Env     map[string]string `json:"-"`
	FileEnv map[string]string `json:"-"`
	Config  config.App        `json:"config"`
	// Branch is the git branch the app runs and BranchURL its page on the git host, each empty
	// when unknown (see gitSource).
	Branch    string `json:"branch,omitempty"`
	BranchURL string `json:"branch_url,omitempty"`
	// GitConnected reports a checkout remote or packed release repository, even without a branch URL.
	GitConnected bool `json:"git_connected,omitempty"`
}

// CronJob is one scheduled command with its schedule parsed once at load time.
type CronJob struct {
	Command  Command           `json:"command"`
	Schedule schedule.Schedule `json:"-"`
	Timeout  time.Duration     `json:"timeout"`
	Overlap  bool              `json:"overlap"`
	Disabled bool              `json:"disabled"`
}

// Hook is one named one-shot command with its command line parsed once at load time. Pull marks
// the `deploy: true` shorthand, whose git command authenticates with tokens.github.
type Hook struct {
	Command  Command       `json:"command"`
	Timeout  time.Duration `json:"timeout"`
	Restart  bool          `json:"restart"`
	Overlap  bool          `json:"overlap"`
	Disabled bool          `json:"disabled"`
	Pull     bool          `json:"pull,omitempty"`
}

// Step is one lifecycle command with its timeout resolved.
type Step struct {
	Command Command       `json:"command"`
	Timeout time.Duration `json:"timeout"`
}

type ScanError struct {
	Name string `json:"name"`
	Err  error  `json:"-"`
}

func (e ScanError) Error() string { return e.Name + ": " + e.Err.Error() }

// Discover loads every app the root config describes: the config's own folder in single mode,
// otherwise each entry of the apps directory. Entries are walked in name order, which decides
// port assignment and host-conflict precedence.
func Discover(cfg config.Config) ([]*App, []error, error) {
	var found []*App
	var invalid []error
	if cfg.Dev() {
		name := filepath.Base(cfg.Dir)
		app, err := loadRootApp(cfg, name)
		if err != nil {
			invalid = append(invalid, ScanError{Name: name, Err: err})
		} else {
			found = append(found, app)
		}
		return resolveHosts(found, invalid)
	}
	entries, err := appNames(cfg.Apps)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range entries {
		app, err := loadChildApp(cfg, name, filepath.Join(cfg.Apps, name))
		if err != nil {
			invalid = append(invalid, ScanError{Name: name, Err: err})
			continue
		}
		found = append(found, app)
	}
	return resolveHosts(found, invalid)
}

// appNames lists the entries of an apps directory, in name order, skipping dotfiles.
func appNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("apps directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// Lookup discovers every app and returns the one called name, or its scan error when it is
// invalid, so the CLI and the console print the same thing for the same app.
func Lookup(cfg config.Config, name string) (*App, error) {
	found, invalid, err := Discover(cfg)
	if err != nil {
		return nil, err
	}
	for _, scanErr := range invalid {
		if scanError, ok := scanErr.(ScanError); ok && scanError.Name == name {
			return nil, scanErr
		}
	}
	for _, app := range found {
		if app.Name == name {
			return app, nil
		}
	}
	return nil, fmt.Errorf("unknown app %q", name)
}

// resolveHosts settles host patterns across apps. A pattern claimed with `host` beats one derived
// from subdomain + domain, so the subdomain app yields it; two claims of the same kind go to the
// app that sorts first and refuse the other. An app refused over its subdomain hosts still
// reserved its own `host` patterns, so the claim runs again without it.
func resolveHosts(found []*App, invalid []error) ([]*App, []error, error) {
	for {
		valid, configs, clashes, late := claimHosts(found)
		if len(late) == 0 {
			for _, app := range valid {
				app.Config = configs[app]
			}
			return valid, append(invalid, clashes...), nil
		}
		dropped := map[string]bool{}
		for _, err := range late {
			invalid = append(invalid, err)
			dropped[err.Name] = true
		}
		found = slices.DeleteFunc(found, func(app *App) bool { return dropped[app.Name] })
	}
}

// claimHosts is one pass of resolveHosts: `host` patterns first, then subdomain patterns, which
// yield to them. It works on copies of the configs, so a pass that has to run again leaves every
// config as it was loaded. clashes are refusals over `host` patterns, late those over subdomains.
func claimHosts(found []*App) (valid []*App, configs map[*App]config.App, clashes []error, late []ScanError) {
	explicit := map[string]string{}
	claimed := make([]*App, 0, len(found))
	for _, app := range found {
		conflict := ""
		for _, host := range hostsOf(app.Config, false) {
			if owner := explicit[config.NormalizePattern(host)]; owner != "" {
				conflict = fmt.Sprintf("host pattern %q is already owned by %s", host, owner)
				break
			}
		}
		if conflict != "" {
			clashes = append(clashes, ScanError{Name: app.Name, Err: errors.New(conflict)})
			continue
		}
		for _, host := range hostsOf(app.Config, false) {
			explicit[config.NormalizePattern(host)] = app.Name
		}
		claimed = append(claimed, app)
	}
	derived := map[string]string{}
	configs = map[*App]config.App{}
	for _, app := range claimed {
		cfg := app.Config
		var err error
		for _, host := range hostsOf(app.Config, true) {
			normalized := config.NormalizePattern(host)
			if owner := explicit[normalized]; owner != "" {
				if err = cfg.Yield(host, owner); err != nil {
					break
				}
				continue
			}
			if owner := derived[normalized]; owner != "" {
				err = fmt.Errorf("host pattern %q is already owned by %s", host, owner)
				break
			}
		}
		if err != nil {
			late = append(late, ScanError{Name: app.Name, Err: err})
			continue
		}
		for _, host := range hostsOf(cfg, true) {
			derived[config.NormalizePattern(host)] = app.Name
		}
		configs[app] = cfg
		valid = append(valid, app)
	}
	// A www/apex redirect is the weakest claim: any pattern of another app covering its hostname
	// takes it. Two apps cannot share one, since its sibling would clash first.
	for _, app := range valid {
		cfg := configs[app]
		cfg.DropRedirects(func(host string) bool {
			return coveredByOther(host, app.Name, explicit) || coveredByOther(host, app.Name, derived)
		})
		configs[app] = cfg
	}
	return valid, configs, clashes, late
}

// coveredByOther reports whether a pattern another app owns matches host.
func coveredByOther(host, app string, owners map[string]string) bool {
	for pattern, owner := range owners {
		if owner == app {
			continue
		}
		if _, ok := config.MatchHost(host, pattern); ok {
			return true
		}
	}
	return false
}

// hostsOf lists an app's host patterns of one kind: derived from a subdomain, or claimed with host.
// A www/apex redirect hostname is no claim and is settled after both.
func hostsOf(cfg config.App, subdomain bool) []string {
	var hosts []string
	for _, web := range cfg.WebProcesses {
		if web.Subdomain == subdomain {
			hosts = append(hosts, web.Claims()...)
		}
	}
	return hosts
}

// loadRootApp re-reads the root file so a rescan in single mode picks up edits to it.
func loadRootApp(cfg config.Config, name string) (*App, error) {
	loaded, err := config.Load(cfg.SourcePath)
	if err != nil {
		return nil, err
	}
	if loaded.App == nil {
		return nil, fmt.Errorf("%s no longer describes an app", cfg.SourcePath)
	}
	return buildApp(name, cfg.Dir, *loaded.App)
}

// loadChildApp loads one entry of the apps directory. The entry path, not its symlink target,
// is the process working directory so a target that is itself a release symlink keeps working.
func loadChildApp(cfg config.Config, name, dir string) (*App, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("app path %q is not a directory", dir)
	}
	path, err := config.FindInDir(dir)
	if err != nil {
		return nil, err
	}
	appCfg, err := config.LoadApp(path, cfg.Defaults)
	if err != nil {
		return nil, err
	}
	return buildApp(name, dir, appCfg)
}

func buildApp(name, dir string, appCfg config.App) (*App, error) {
	commands := make(map[string]Command, len(appCfg.Procfile))
	for name, process := range appCfg.Procfile {
		commands[name] = newCommand(name, process.Command)
	}
	cron, err := buildCron(appCfg.Cron)
	if err != nil {
		return nil, err
	}
	env := minimalEnvironment()
	if _, err := os.Stat(filepath.Join(dir, "mise.toml")); err == nil {
		mise, miseErr := miseEnvironment(dir)
		if miseErr != nil {
			return nil, miseErr
		}
		merge(env, mise)
	}
	// .env and .env.local stay separate so they can outrank the config env when the process
	// environment is assembled (see supervisor.environment).
	fileEnv := map[string]string{}
	for _, filename := range []string{".env", ".env.local"} {
		values, envErr := loadEnv(filepath.Join(dir, filename))
		if envErr != nil {
			return nil, envErr
		}
		merge(fileEnv, values)
	}
	branch, branchURL, gitConnected := gitSource(dir, fileEnv)
	_, checkout := findGitDir(dir)
	hooks := buildHooks(appCfg.Hooks, appCfg.Branch)
	if _, configured := hooks["deploy"]; !configured && gitConnected && checkout {
		hooks["deploy"] = buildHook("deploy", config.PullHook(), appCfg.Branch)
	}
	lifecycle := buildLifecycle(appCfg.Lifecycle)
	// Only a checkout of its own: an app inside a larger repository never stashes or switches it.
	if appCfg.Branch != "" && checkout {
		lifecycle["branch"] = Step{Command: newCommand("branch", config.BranchCommand(appCfg.Branch)), Timeout: config.DefaultLifecycleTimeout}
	}
	return &App{Name: name, Dir: dir, Commands: commands, Cron: cron, Hooks: hooks, Lifecycle: lifecycle, Env: env, FileEnv: fileEnv, Config: appCfg, Branch: branch, BranchURL: branchURL, GitConnected: gitConnected}, nil
}

// newCommand is one config command line, run through /bin/sh -c; config has already refused an
// empty one.
func newCommand(name, line string) Command {
	return Command{Name: name, Line: strings.TrimSpace(line)}
}

// buildCron parses every schedule once so the supervisor only has to work with next run times.
func buildCron(jobs map[string]config.CronJob) (map[string]CronJob, error) {
	result := make(map[string]CronJob, len(jobs))
	for name, job := range jobs {
		parsed, err := schedule.Parse(job.Schedule)
		if err != nil {
			return nil, fmt.Errorf("cron.%s.schedule: %w", name, err)
		}
		result[name] = CronJob{
			Command:  newCommand(name, job.Command),
			Schedule: parsed,
			Timeout:  job.Timeout.Value(),
			Overlap:  job.Overlap,
			Disabled: job.Disabled,
		}
	}
	return result, nil
}

// buildHooks parses every hook command once; hooks have no schedule, they only fire on a ping.
// branch is the app's `branch`, which a pull hook refuses to pull on another one.
func buildHooks(hooks map[string]config.Hook, branch string) map[string]Hook {
	result := make(map[string]Hook, len(hooks))
	for name, hook := range hooks {
		result[name] = buildHook(name, hook, branch)
	}
	return result
}

func buildHook(name string, hook config.Hook, branch string) Hook {
	if hook.Pull {
		hook.Command = config.PullCommand(branch)
	}
	return Hook{
		Command:  newCommand(name, hook.Command),
		Timeout:  hook.Timeout.Value(),
		Restart:  hook.Restart,
		Overlap:  hook.Overlap,
		Disabled: hook.Disabled,
		Pull:     hook.Pull,
	}
}

// buildLifecycle parses each step once and applies the default timeout.
func buildLifecycle(steps map[string]config.LifecycleCommand) map[string]Step {
	result := make(map[string]Step, len(steps))
	for name, step := range steps {
		timeout := step.Timeout.Value()
		if timeout == 0 {
			timeout = config.DefaultLifecycleTimeout
		}
		result[name] = Step{Command: newCommand(name, step.Command), Timeout: timeout}
	}
	return result
}

func loadEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			if value[0] == '\'' {
				value = value[1 : len(value)-1]
			} else {
				decoded, decodeErr := strconv.Unquote(value)
				if decodeErr != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, decodeErr)
				}
				value = decoded
			}
		}
		result[key] = value
	}
	return result, scanner.Err()
}

func validEnvKey(key string) bool {
	if key == "" || !(key[0] == '_' || key[0] >= 'A' && key[0] <= 'Z' || key[0] >= 'a' && key[0] <= 'z') {
		return false
	}
	for i := 1; i < len(key); i++ {
		character := key[i]
		if !(character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func minimalEnvironment() map[string]string {
	result := map[string]string{}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "TZ", "PATH", "USER"} {
		if value, ok := os.LookupEnv(key); ok {
			result[key] = value
		}
	}
	return result
}

func miseEnvironment(dir string) (map[string]string, error) {
	cmd := exec.Command("mise", "env", "--json")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mise env: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("mise env JSON: %w", err)
	}
	return values, nil
}

func merge(target, source map[string]string) {
	for key, value := range source {
		target[key] = value
	}
}
