package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/fsutil"
	"dboss/internal/supervisor"
	"dboss/internal/sysinfo"
	"gopkg.in/yaml.v3"
)

// syncManifestName is the file in the remote app folder listing what the last sync shipped. It is
// what lets apply delete a file the app dropped without ever touching one that only lives on the box.
const syncManifestName = ".dboss-sync"

// deployHook is the app hook `deploy git` pings; `hooks: {deploy: true}` is the pull-and-restart.
const deployHook = "deploy"

// deployWait bounds how long `deploy git` waits for the pull and the restart.
const deployWait = 15 * time.Minute

// deployPoll is how often `deploy git` asks for the hook state; a var so tests can poll faster.
var deployPoll = time.Second

var deployClient = &http.Client{Timeout: 30 * time.Second}

// syncProtected are never removed by apply, even when an earlier sync shipped them: they are
// server-only by convention and may have been placed on the box since.
var syncProtected = []string{syncManifestName, config.LocalFileName, config.ServerLocalFileName, ".env.local"}

func (c CLI) deploy(args []string) error {
	if len(args) == 0 {
		return c.help(c.Out, "deploy")
	}
	switch args[0] {
	case "sync":
		return c.deploySync(args[1:])
	case "git":
		return c.deployGit(args[1:])
	case "apply":
		return c.deployApply(args[1:])
	case "pull":
		return c.deployPull(args[1:])
	case "ip":
		return c.deployIP(args[1:])
	case "help":
		return c.help(c.Out, "deploy")
	default:
		return fmt.Errorf("unknown deploy subcommand %q (use sync, git or pull)", args[0])
	}
}

// deploySync ships the tracked files of the app or host folder with rsync, then runs `deploy apply`
// on the box over ssh, which drops what the folder no longer ships and restarts the app or rescans
// the host.
func (c CLI) deploySync(args []string) error {
	set := flag.NewFlagSet("deploy sync", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	app := set.String("app", "", "app name on the box (default: the remote folder name)")
	var dryRun bool
	set.BoolVar(&dryRun, "n", false, "show what would change")
	set.BoolVar(&dryRun, "dry-run", false, "show what would change")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	usage := errors.New("usage: dboss deploy sync [user@host:/path] [--app name] [-n] (in an app folder the target defaults to its server key)")
	if len(operands) > 1 {
		return usage
	}
	here := &workdir{explicit: *configPath}
	configFile, cfg, err := here.load()
	if err != nil {
		return err
	}
	isHost := cfg.App == nil
	var target string
	switch {
	case len(operands) == 1:
		target = operands[0]
	case isHost:
		target = defaultHostTarget(cfg.Dir)
	case cfg.App.Server != "":
		target = cfg.App.ServerTarget(filepath.Base(cfg.Dir))
	default:
		return usage
	}
	host, remotePath, err := config.SplitTarget(target)
	if err != nil {
		return err
	}
	if isHost {
		if *app != "" {
			return fmt.Errorf("--app names an app, and %s is a host config", configFile)
		}
	} else {
		if *app == "" {
			*app = path.Base(remotePath)
		}
		if !config.RemoteSafe.MatchString(*app) {
			return fmt.Errorf("app name %q has characters the remote shell would read", *app)
		}
		if err := checkBranch(cfg.Dir, cfg.App.Branch); err != nil {
			return err
		}
	}
	files, untracked, err := syncManifest(cfg.Dir)
	if err != nil {
		return err
	}
	if isHost {
		if err := checkHostManifest(cfg, configFile, files); err != nil {
			return err
		}
	}
	if untracked > 0 {
		fmt.Fprintf(c.Err, "skipped %d untracked %s (git add to ship them)\n", untracked, plural(untracked, "file"))
	}
	manifest := strings.Join(files, "\n") + "\n"

	rsync := exec.Command("rsync", rsyncArgs(host, remotePath, dryRun)...)
	rsync.Dir = cfg.Dir
	rsync.Stdin = strings.NewReader(manifest)
	rsync.Stdout, rsync.Stderr = c.Out, c.Err
	if err := rsync.Run(); err != nil {
		return fmt.Errorf("rsync: %w", err)
	}
	ssh := exec.Command("ssh", applyArgs(host, remotePath, *app, dryRun)...)
	ssh.Stdin = strings.NewReader(manifest)
	ssh.Stdout, ssh.Stderr = c.Out, c.Err
	if err := ssh.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &exitError{code: exit.ExitCode()}
		}
		return fmt.Errorf("ssh: %w", err)
	}
	if !dryRun {
		fmt.Fprintf(c.Out, "deployed %s: %d %s synced to %s:%s\n", orElse(*app, "host"), len(files), plural(len(files), "file"), host, remotePath)
	}
	return nil
}

// defaultHostTarget is the box a host folder deploys to: its folder name is the ssh destination,
// so ~/dev/servers/deb1 goes to `deb1`, with the user from ~/.ssh/config.
func defaultHostTarget(dir string) string {
	return filepath.Base(dir) + ":" + config.DefaultServerDir
}

// checkHostManifest refuses a host sync that does not carry the host file, or that would ship into
// the apps or runtime folder, which belong to the box.
func checkHostManifest(cfg config.Config, configFile string, files []string) error {
	base, err := filepath.Rel(cfg.Dir, filepath.Join(filepath.Dir(configFile), config.ServerFileName))
	if err != nil {
		return err
	}
	if !slices.Contains(files, filepath.ToSlash(base)) {
		return fmt.Errorf("%s is not tracked; git add it, a host sync ships exactly that file", base)
	}
	for _, owned := range []string{cfg.Apps, cfg.RuntimeDir} {
		rel, err := filepath.Rel(cfg.Dir, owned)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		prefix := filepath.ToSlash(rel) + "/"
		for _, name := range files {
			if strings.HasPrefix(name, prefix) {
				return fmt.Errorf("cannot ship %s: %s belongs to the box", name, prefix)
			}
		}
	}
	return nil
}

// syncManifest lists the files git tracks in dir, relative to it, and counts the untracked files
// git does not ignore, so a missing `git add` is visible. Untracked files are never shipped: a file
// the box needs that git does not carry is placed there on purpose.
func syncManifest(dir string) ([]string, int, error) {
	tracked, err := gitList(dir, "ls-files", "-z", "--cached", "--recurse-submodules")
	if err != nil {
		return nil, 0, err
	}
	files := make([]string, 0, len(tracked))
	for _, name := range tracked {
		if name == syncManifestName {
			continue
		}
		if strings.ContainsAny(name, "\n\r") {
			return nil, 0, fmt.Errorf("cannot ship %q: the name has a line break", name)
		}
		// A tracked file deleted from the working tree is not shipped, and apply then removes it.
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, 0, err
		}
		files = append(files, name)
	}
	untracked, err := gitList(dir, "ls-files", "-z", "--others", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		return nil, 0, err
	}
	return files, len(untracked), nil
}

// checkBranch refuses to ship a checkout that is not on the app's `branch`.
func checkBranch(dir, branch string) error {
	if branch == "" {
		return nil
	}
	current, err := gitList(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || len(current) != 1 {
		return fmt.Errorf("branch: %s requires a checkout on branch %q, this one has none", filepath.Join(dir, config.FileName), branch)
	}
	if got := strings.TrimSpace(current[0]); got != branch {
		return fmt.Errorf("branch: this checkout is on %q, the app requires %q; switch branches first", got, branch)
	}
	return nil
}

func gitList(dir string, args ...string) ([]string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return nil, fmt.Errorf("deploy sync ships the files git tracks, and %s is not in a git repository", dir)
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// rsyncArgs copies exactly the listed files. rsync's own --delete is never used: openrsync, the
// macOS default, removes gitignored files on the receiver with it. apply deletes instead.
func rsyncArgs(host, remotePath string, dryRun bool) []string {
	args := []string{"-az", "--files-from=-"}
	if dryRun {
		args = append(args, "-n", "-v")
	}
	return append(args, "./", host+":"+remotePath+"/")
}

// applyArgs builds the box's half of sync; a host sync has no app.
func applyArgs(host, remotePath, app string, dryRun bool) []string {
	args := []string{host, "dboss", "deploy", "apply", remotePath}
	if app != "" {
		args = append(args, "--app", app)
	}
	if dryRun {
		args = append(args, "-n")
	}
	return args
}

// deployApply is the box's half of `deploy sync`: it reads the new manifest on stdin, removes the
// files the previous manifest listed and this one does not and records the new manifest. In an app
// folder it then rescans and restarts the app; in a host folder it validates the shipped host file
// first and only rescans, since a daemon restart takes every app down.
func (c CLI) deployApply(args []string) error {
	set := flag.NewFlagSet("deploy apply", flag.ContinueOnError)
	set.SetOutput(c.Err)
	app := set.String("app", "", "app name (default: the folder name)")
	socket := set.String("socket", "", "control socket")
	var dryRun bool
	set.BoolVar(&dryRun, "n", false, "list what would be removed")
	set.BoolVar(&dryRun, "dry-run", false, "list what would be removed")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 1 || !filepath.IsAbs(operands[0]) {
		return errors.New("usage: dboss deploy apply </absolute/folder> [--app name] [-n]")
	}
	root := filepath.Clean(operands[0])
	// A mistyped path must delete nothing, so the folder has to be a host or an app.
	var hostBase string
	if found, err := config.FindServerInDir(root); err == nil {
		if *app != "" {
			return fmt.Errorf("--app names an app, and %s is a host folder", root)
		}
		hostBase = filepath.Join(filepath.Dir(found), config.ServerFileName)
	} else if !errors.Is(err, config.ErrNoConfig) {
		return err
	} else if configFile, err := config.FindInDir(root); err != nil {
		return fmt.Errorf("%s holds no app or host config: %w", root, err)
	} else if cfg, err := config.Load(configFile); err != nil {
		return err
	} else if cfg.App == nil {
		return fmt.Errorf("%s is a host config, not an app", configFile)
	} else if *app == "" {
		*app = filepath.Base(root)
	}
	next, err := readManifest(c.In)
	if err != nil {
		return err
	}
	var previous []string
	if data, err := os.ReadFile(filepath.Join(root, syncManifestName)); err == nil {
		if previous, err = readManifest(bytes.NewReader(data)); err != nil {
			return fmt.Errorf("%s: %w", syncManifestName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	removed := staleFiles(previous, next)
	if hostBase != "" {
		c.warnLocalOverride(hostBase)
	}
	if dryRun {
		for _, name := range removed {
			fmt.Fprintf(c.Out, "would remove %s\n", name)
		}
		return nil
	}
	if hostBase != "" {
		// The manifest stays at the previous sync, so the next one still knows what to remove.
		if _, err := config.Load(hostBase); err != nil {
			return fmt.Errorf("the shipped %s is invalid, the daemon keeps its running config: %w", config.ServerFileName, err)
		}
	}
	for _, name := range removed {
		if err := removeShipped(root, name); err != nil {
			fmt.Fprintf(c.Err, "keep %s: %v\n", name, err)
		}
	}
	if len(removed) > 0 {
		fmt.Fprintf(c.Out, "removed %d %s the folder no longer ships\n", len(removed), plural(len(removed), "file"))
	}
	if err := fsutil.WriteFile(filepath.Join(root, syncManifestName), []byte(strings.Join(next, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	var scan []string
	if *socket != "" {
		scan = []string{"--socket", *socket}
	}
	if hostBase != "" {
		// Rescan applies the app-level keys and lists the host keys that wait for a daemon restart.
		return c.remote("rescan", append(scan, "-c", hostBase))
	}
	// The synced dboss.yaml may differ from the spec the daemon holds; restart alone keeps the old one.
	if err := c.remote("rescan", scan); err != nil {
		return fmt.Errorf("rescan: %w", err)
	}
	restart := append([]string{*app}, scan...)
	if err := c.remote("restart", restart); err != nil {
		return fmt.Errorf("restart %s: %w", *app, err)
	}
	return nil
}

// warnLocalOverride says when the box's dboss-server.local.yaml shadows the shipped host file: the
// local file replaces it entirely, so nothing a sync ships applies until the local file is gone.
func (c CLI) warnLocalOverride(base string) {
	local := config.LocalFor(base)
	localData, err := os.ReadFile(local)
	if err != nil {
		return
	}
	baseData, _ := os.ReadFile(base)
	fmt.Fprintf(c.Err, "warning: %s replaces %s entirely, so the shipped file does not apply until it is removed\n", local, filepath.Base(base))
	if keys := differingKeys(baseData, localData); len(keys) > 0 {
		fmt.Fprintf(c.Err, "  it differs at: %s\n", strings.Join(keys, ", "))
	}
}

// differingKeys lists the dotted key paths whose values differ between two YAML documents; nil
// when either does not parse.
func differingKeys(a, b []byte) []string {
	var left, right any
	if yaml.Unmarshal(a, &left) != nil || yaml.Unmarshal(b, &right) != nil {
		return nil
	}
	var keys []string
	diffYAML("", left, right, &keys)
	sort.Strings(keys)
	return keys
}

func diffYAML(prefix string, a, b any, keys *[]string) {
	left, leftMap := a.(map[string]any)
	right, rightMap := b.(map[string]any)
	if !leftMap || !rightMap {
		if !reflect.DeepEqual(a, b) {
			*keys = append(*keys, orElse(prefix, "(the whole file)"))
		}
		return
	}
	seen := map[string]bool{}
	for _, side := range []map[string]any{left, right} {
		for key := range side {
			if seen[key] {
				continue
			}
			seen[key] = true
			name := key
			if prefix != "" {
				name = prefix + "." + key
			}
			diffYAML(name, left[key], right[key], keys)
		}
	}
}

// deployPull copies a box's live host config into the current folder as dboss-server.yaml, so the
// host can be kept in a repository and shipped back with `deploy sync`. The live file is the box's
// dboss-server.local.yaml when it has one, since that replaces the base entirely.
func (c CLI) deployPull(args []string) error {
	set := flag.NewFlagSet("deploy pull", flag.ContinueOnError)
	set.SetOutput(c.Err)
	force := set.Bool("force", false, "overwrite a local dboss-server.yaml that differs from the box")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) > 1 {
		return errors.New("usage: dboss deploy pull [user@host:/path] [--force]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := config.FindInDir(dir); err == nil {
		return fmt.Errorf("%s is an app folder; pull fetches a host config", dir)
	} else if !errors.Is(err, config.ErrNoConfig) {
		return err
	}
	target := defaultHostTarget(dir)
	if len(operands) == 1 {
		target = operands[0]
	}
	host, remotePath, err := config.SplitTarget(target)
	if err != nil {
		return err
	}
	source := path.Join(remotePath, config.ServerLocalFileName)
	data, err := remoteFile(host, source)
	overridden := err == nil
	if errors.Is(err, os.ErrNotExist) {
		source = path.Join(remotePath, config.ServerFileName)
		data, err = remoteFile(host, source)
	}
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, config.ServerFileName)
	if current, err := os.ReadFile(dest); err == nil {
		if bytes.Equal(current, data) {
			fmt.Fprintf(c.Out, "%s matches %s:%s\n", config.ServerFileName, host, source)
			return nil
		}
		if !*force {
			return fmt.Errorf("%s differs from %s:%s; commit or drop the local edits, or pass --force", dest, host, source)
		}
	}
	// Host files carry tokens and passwords.
	if err := fsutil.WriteFile(dest, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "pulled %s:%s into %s\n", host, source, dest)
	if overridden {
		fmt.Fprintf(c.Err, "the box runs from %s; after the first deploy sync, remove it there so the shipped file applies\n", config.ServerLocalFileName)
	}
	return nil
}

// deployIP is the box's half of `cf dns`: it prints the address the app's DNS records point at,
// the same public IP the console's Sys tab shows.
func (c CLI) deployIP(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dboss deploy ip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	address, err := sysinfo.PublicIP(ctx)
	if err != nil {
		return fmt.Errorf("this box has no routable address of its own and asking for it failed: %w", err)
	}
	fmt.Fprintln(c.Out, address)
	return nil
}

// remoteFile reads one file on the box over ssh; os.ErrNotExist when it is not there.
func remoteFile(host, name string) ([]byte, error) {
	command := exec.Command("ssh", host, "cat", name)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err == nil {
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() != 255 && strings.Contains(stderr.String(), "No such file") {
		return nil, os.ErrNotExist
	}
	return nil, fmt.Errorf("ssh %s cat %s: %w: %s", host, name, err, strings.TrimSpace(stderr.String()))
}

// readManifest reads one relative path per line and refuses anything that would leave the app
// folder, so a manifest can never name a file outside it.
func readManifest(in io.Reader) ([]string, error) {
	var names []string
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		name := scanner.Text()
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("manifest entry %q leaves the app folder", name)
		}
		names = append(names, filepath.Clean(name))
	}
	return names, scanner.Err()
}

// staleFiles is what previous listed and next no longer does, minus the protected names, sorted.
func staleFiles(previous, next []string) []string {
	keep := make(map[string]bool, len(next))
	for _, name := range next {
		keep[name] = true
	}
	var stale []string
	for _, name := range previous {
		if !keep[name] && !slices.Contains(syncProtected, filepath.Base(name)) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	return slices.Compact(stale)
}

// removeShipped removes one file under root without following a symlink anywhere on its path,
// then every parent directory the removal left empty.
func removeShipped(root, name string) error {
	parts := strings.Split(name, string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("a parent is not a directory")
		}
	}
	if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
		if os.Remove(filepath.Join(root, dir)) != nil {
			break
		}
	}
	return nil
}

// deployGit pings the app's deploy hook on the box with tokens.dboss and waits until the pull and
// the restart are done, printing the hook's output.
func (c CLI) deployGit(args []string) error {
	set := flag.NewFlagSet("deploy git", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	app := set.String("app", "", "app name on the box (default: the current folder's app)")
	token := set.String("token", "", "tokens.dboss of the box (default: $DBOSS_TOKEN)")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 1 {
		return errors.New("usage: dboss deploy git <https://management.host> [--app name] [--token t]")
	}
	base, err := url.Parse(strings.TrimRight(operands[0], "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL of the box's management host", operands[0])
	}
	if *token == "" {
		*token = os.Getenv("DBOSS_TOKEN")
	}
	if *token == "" {
		return errors.New("pass --token or set DBOSS_TOKEN to the box's tokens.dboss")
	}
	if *app == "" {
		here := &workdir{explicit: *configPath}
		if *app, err = here.app(nil); err != nil {
			return err
		}
	}
	hookURL := base.String() + "/hooks/" + url.PathEscape(*app) + "/" + deployHook

	status, body, err := hookRequest(http.MethodPost, hookURL, *token)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusAccepted:
	case http.StatusNotFound:
		return fmt.Errorf("%s has no %s hook on %s: add `hooks: {deploy: true}` to its dboss.yaml", *app, deployHook, base.Host)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s rejected the token", base.Host)
	default:
		return fmt.Errorf("%s answered %d: %s", base.Host, status, hookErrorText(body))
	}
	fmt.Fprintf(c.Err, "deploying %s on %s\n", *app, base.Host)

	deadline := time.Now().Add(deployWait)
	for {
		time.Sleep(deployPoll)
		status, body, err := hookRequest(http.MethodGet, hookURL, *token)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s answered %d: %s", base.Host, status, hookErrorText(body))
		}
		var info supervisor.HookInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return fmt.Errorf("%s: %w", base.Host, err)
		}
		if info.Running || info.Restarting {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s is still deploying after %s; see dboss hooks %s on the box", *app, deployWait, *app)
			}
			continue
		}
		if output := strings.TrimRight(info.Output, "\n"); output != "" {
			fmt.Fprintln(c.Out, output)
		}
		if info.LastExit != 0 || info.LastError != "" {
			fmt.Fprintf(c.Err, "dboss: deploy of %s failed: %s\n", *app, orElse(info.LastError, fmt.Sprintf("exit code %d", info.LastExit)))
			return &exitError{code: max(info.LastExit, 1)}
		}
		fmt.Fprintf(c.Out, "deployed %s: pulled and restarted\n", *app)
		return nil
	}
}

func hookRequest(method, target, token string) (int, []byte, error) {
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := deployClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, body, err
}

// hookErrorText is the error of a JSON answer, else the trimmed body.
func hookErrorText(body []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) == nil && answer.Error != "" {
		return answer.Error
	}
	return strings.TrimSpace(string(body))
}

func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

func orElse(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
