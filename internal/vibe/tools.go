package vibe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"dboss/internal/fault"
	"dboss/internal/git"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
)

const (
	// maxResult caps what one tool call hands back to a model.
	maxResult = 20000
	// maxEntryResult caps the result a chat line keeps for the page.
	maxEntryResult = 4000
	maxListFiles   = 2000
	maxSearchHits  = 200
	maxReadLines   = 2000
	defaultRead    = 400
	maxRunTimeout  = 5 * time.Minute
	maxReadBytes   = 2 << 20
)

// param is one argument of a tool.
type param struct {
	Name, Type, Desc string
	Required         bool
}

// Tool is one capability the chat and MCP share. Both render their tool lists from tools, so
// they can never disagree.
type Tool struct {
	Name        string
	Description string
	Params      []param
	// Changes marks a tool that can change the app, so the page re-reads the working tree and
	// reloads the frame after it.
	Changes bool
	Run     func(ctx context.Context, c *call, args arguments) (string, error)
}

// Schema is the tool's JSON Schema input.
func (t Tool) Schema() map[string]any {
	properties := map[string]any{}
	required := []string{}
	for _, p := range t.Params {
		properties[p.Name] = map[string]any{"type": p.Type, "description": p.Desc}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

// arguments is a tool call's decoded input.
type arguments map[string]any

func (a arguments) str(name string) string {
	value, _ := a[name].(string)
	return value
}

func (a arguments) num(name string, fallback int) int {
	if value, ok := a[name].(float64); ok {
		return int(value)
	}
	return fallback
}

func (a arguments) flag(name string) bool {
	value, _ := a[name].(bool)
	return value
}

// call is the context of one tool call: which harness, who acts, and what the turn did so far.
type call struct {
	s         *Service
	app       string
	web       string
	actor     string
	harness   *harness
	external  bool
	wrote     bool
	restarted bool
}

// snapshot is the app's live snapshot, read fresh for every tool that needs it.
func (c *call) snapshot() (supervisor.Snapshot, error) {
	result, err := c.s.ops.Do(ops.Request{Method: ops.ActionStatus, App: c.app})
	if err != nil {
		return supervisor.Snapshot{}, err
	}
	return result.(supervisor.Snapshot), nil
}

var tools = []Tool{
	{Name: "app_info", Description: "The app's name, folder, URLs, processes and their state, git branch and uncommitted change count. Call it first.", Run: appInfo},
	{Name: "list_files", Description: "Files of the app: tracked plus untracked ones git does not ignore. An optional glob (*.rb, app/**) or substring narrows the list.", Params: []param{{Name: "pattern", Type: "string", Desc: "glob or substring to filter paths by"}}, Run: listFiles},
	{Name: "read_file", Description: "Read a text file with line numbers. Use offset and limit to page through a large one.", Params: []param{{Name: "path", Type: "string", Desc: "path relative to the app folder", Required: true}, {Name: "offset", Type: "integer", Desc: "first line to read, 1-based"}, {Name: "limit", Type: "integer", Desc: "lines to read, default 400"}}, Run: readFile},
	{Name: "write_file", Description: "Create a file or replace its whole content. Parent folders are created.", Params: []param{{Name: "path", Type: "string", Desc: "path relative to the app folder", Required: true}, {Name: "content", Type: "string", Desc: "the complete new content", Required: true}}, Changes: true, Run: writeFile},
	{Name: "edit_file", Description: "Replace an exact string in a file. old_string must match exactly once (including whitespace) unless replace_all is set; give enough surrounding lines to make it unique.", Params: []param{{Name: "path", Type: "string", Desc: "path relative to the app folder", Required: true}, {Name: "old_string", Type: "string", Desc: "exact text to replace", Required: true}, {Name: "new_string", Type: "string", Desc: "replacement text", Required: true}, {Name: "replace_all", Type: "boolean", Desc: "replace every occurrence"}}, Changes: true, Run: editFile},
	{Name: "search", Description: "Search file contents with a regular expression; answers path:line: text.", Params: []param{{Name: "pattern", Type: "string", Desc: "regular expression", Required: true}, {Name: "path", Type: "string", Desc: "folder or file to search in, default the whole app"}}, Run: search},
	{Name: "run", Description: "Run a shell command in the app folder with the app's environment (install packages, run migrations or tests). Answers the exit code and the output.", Params: []param{{Name: "command", Type: "string", Desc: "shell command line", Required: true}, {Name: "timeout_seconds", Type: "integer", Desc: "kill after this long, default 60, at most 300"}}, Changes: true, Run: run},
	{Name: "restart", Description: "Restart the app, or one process, and wait until it serves again. Needed after a change the app does not reload on its own.", Params: []param{{Name: "process", Type: "string", Desc: "one procfile process; empty restarts the whole app"}}, Changes: true, Run: restart},
	{Name: "logs", Description: "Recent log rows of the app's processes and log files, oldest first.", Params: []param{{Name: "query", Type: "string", Desc: "full-text search"}, {Name: "process", Type: "string", Desc: "one process"}, {Name: "level", Type: "string", Desc: "debug, info, warn or error"}, {Name: "lines", Type: "integer", Desc: "rows, default 80"}}, Run: logs},
	{Name: "exceptions", Description: "Unresolved exception groups of the last day with their last message and stack dump.", Run: exceptions},
	{Name: "http_get", Description: "GET a path on the app as a browser would, through dboss. Answers the status, content type and body. Use it to verify a change.", Params: []param{{Name: "path", Type: "string", Desc: "path and query, e.g. /cart?id=1", Required: true}}, Run: httpGet},
	{Name: "git_status", Description: "Uncommitted changes with line counts, and the commits not pushed yet.", Run: gitStatus},
	{Name: "commit", Description: "Commit every uncommitted change (ignored files never). Use a short imperative subject.", Params: []param{{Name: "message", Type: "string", Desc: "commit message", Required: true}}, Changes: true, Run: commit},
}

func findTool(name string) (Tool, bool) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// runTool runs one call, records it in the chat and returns what the model gets back.
func (s *Service) runTool(ctx context.Context, c *call, name string, raw json.RawMessage) (string, bool) {
	tool, ok := findTool(name)
	args := arguments{}
	var err error
	if !ok {
		err = fault.Invalidf("unknown tool %q", name)
	} else if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if jsonErr := json.Unmarshal(raw, &args); jsonErr != nil {
			err = fault.Invalidf("arguments are not a JSON object: %v", jsonErr)
		}
	}
	result := ""
	if err == nil {
		for _, p := range tool.Params {
			if p.Required {
				if _, present := args[p.Name]; !present {
					err = fault.Invalidf("missing argument %q", p.Name)
					break
				}
			}
		}
	}
	if err == nil {
		result, err = tool.Run(ctx, c, args)
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	result = capText(result, maxResult)
	kind := KindTool
	if c.external {
		kind = KindExternal
	}
	entry := Entry{Kind: kind, Tool: name, Summary: summary(name, args), Result: capText(result, maxEntryResult), OK: err == nil, Path: changedPath(tool, args)}
	c.harness.add(entry)
	if tool.Changes && err == nil {
		c.wrote = true
		c.harness.notifyGit()
	}
	return result, err == nil
}

// summary is the one line a chat shows for a call.
func summary(name string, args arguments) string {
	for _, key := range []string{"path", "command", "pattern", "query", "message", "process"} {
		if value := args.str(key); value != "" {
			return firstLine(value, 120)
		}
	}
	return ""
}

func changedPath(tool Tool, args arguments) string {
	if tool.Name == "write_file" || tool.Name == "edit_file" {
		return filepath.ToSlash(filepath.Clean(args.str("path")))
	}
	return ""
}

func firstLine(text string, limit int) string {
	line, _, _ := strings.Cut(text, "\n")
	if len(line) > limit {
		line = line[:limit] + "..."
	}
	return line
}

// capText keeps the head of a long text and says how much was cut.
func capText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + fmt.Sprintf("\n[... %d more characters cut]", len(text)-limit)
}

// confine resolves a path the model gave inside the app folder. Symlinks are followed before the
// check, and the .git folder and dboss's runtime folder are never reachable.
func (s *Service) confine(dir, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fault.Invalidf("path is required")
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(rel) {
		for _, base := range []string{dir, realDir} {
			if inside, err := filepath.Rel(base, rel); err == nil && !strings.HasPrefix(inside, "..") {
				rel = inside
				break
			}
		}
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fault.Invalidf("%s is outside the app folder", rel)
	}
	real := resolveExisting(filepath.Join(realDir, clean))
	blocked := []string{filepath.Join(realDir, ".git")}
	if s.runtimeDir != "" {
		blocked = append(blocked, resolveExisting(s.runtimeDir))
	}
	if !within(real, realDir) {
		return "", fault.Invalidf("%s is outside the app folder", rel)
	}
	for _, root := range blocked {
		if within(real, root) {
			return "", fault.Invalidf("%s is off limits", rel)
		}
	}
	return clean, nil
}

// resolveExisting follows the symlinks of the longest existing prefix of p and keeps the rest.
func resolveExisting(p string) string {
	rest := ""
	for current := p; ; current = filepath.Dir(current) {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(real, rest)
		}
		if parent := filepath.Dir(current); parent == current {
			return p
		}
		rest = filepath.Join(filepath.Base(current), rest)
	}
}

func within(p, root string) bool {
	inside, err := filepath.Rel(root, p)
	return err == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator))
}

func appInfo(_ context.Context, c *call, _ arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "app: %s\nfolder: %s\nstate: %s\n", app.Name, app.Dir, app.State)
	if app.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", app.Branch)
	}
	for _, url := range app.URLs {
		fmt.Fprintf(&b, "url (%s): %s\n", url.Process, url.URL)
	}
	for _, web := range app.WebProcesses {
		fmt.Fprintf(&b, "web process %s: hosts %s\n", web.Name, strings.Join(web.Hosts, ", "))
	}
	for _, process := range app.Processes {
		fmt.Fprintf(&b, "process %s: %s\n", process.Name, process.State)
	}
	if status, err := git.Status(context.Background(), app.Dir); err == nil {
		fmt.Fprintf(&b, "uncommitted changes: %d\nunpushed commits: %d\n", len(status.Changes), status.Ahead)
	}
	if app.Error != "" {
		fmt.Fprintf(&b, "last error: %s\n", app.Error)
	}
	return b.String(), nil
}

// appFiles lists the app's files: git's view when the folder is a checkout, else a walk that
// skips the usual dependency and runtime folders.
func (s *Service) appFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard", "-z")
	if out, err := cmd.Output(); err == nil {
		files := []string{}
		for _, name := range strings.Split(string(out), "\x00") {
			if name != "" {
				files = append(files, name)
			}
		}
		slices.Sort(files)
		return slices.Compact(files), nil
	}
	skip := map[string]bool{".git": true, "node_modules": true, ".dboss": true, "tmp": true, "log": true, "vendor": true, "dist": true, "build": true}
	files := []string{}
	err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != dir && skip[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, filepath.ToSlash(rel))
		if len(files) > maxListFiles*5 {
			return filepath.SkipAll
		}
		return nil
	})
	return files, err
}

func matchPath(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return strings.Contains(name, pattern)
	}
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(name, prefix+"/")
	}
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	ok, _ := path.Match(pattern, path.Base(name))
	return ok
}

func listFiles(_ context.Context, c *call, args arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	files, err := c.s.appFiles(app.Dir)
	if err != nil {
		return "", err
	}
	pattern := args.str("pattern")
	var matched []string
	for _, name := range files {
		if matchPath(pattern, name) {
			matched = append(matched, name)
		}
	}
	if len(matched) == 0 {
		return "no files match", nil
	}
	text := strings.Join(matched[:min(len(matched), maxListFiles)], "\n")
	if len(matched) > maxListFiles {
		text += fmt.Sprintf("\n[... %d more files; narrow with pattern]", len(matched)-maxListFiles)
	}
	return text, nil
}

func readFile(_ context.Context, c *call, args arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	name, err := c.s.confine(app.Dir, args.str("path"))
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(app.Dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Stat(name)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fault.Invalidf("%s is a folder; use list_files", name)
	}
	if info.Size() > maxReadBytes {
		return "", fault.Invalidf("%s is %d KB, too large to read; search it instead", name, info.Size()>>10)
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return fmt.Sprintf("%s is a binary file of %d bytes", name, len(data)), nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	offset := max(args.num("offset", 1), 1)
	limit := min(max(args.num("limit", defaultRead), 1), maxReadLines)
	if offset > len(lines) {
		return fmt.Sprintf("%s has %d lines", name, len(lines)), nil
	}
	var b strings.Builder
	end := min(offset-1+limit, len(lines))
	for index := offset - 1; index < end; index++ {
		fmt.Fprintf(&b, "%6d\t%s\n", index+1, lines[index])
		if b.Len() > maxResult {
			end = index + 1
			break
		}
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "[%d of %d lines; continue with offset=%d]\n", end, len(lines), end+1)
	}
	return b.String(), nil
}

// writeAppFile replaces name inside the app folder through a temp file and a rename, keeping an
// existing file's mode.
func writeAppFile(dir, name string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	mode := os.FileMode(0o644)
	if info, err := root.Stat(name); err == nil {
		if info.IsDir() {
			return fault.Invalidf("%s is a folder", name)
		}
		mode = info.Mode().Perm()
	}
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	temp := name + ".dboss-tmp"
	if err := root.WriteFile(temp, data, mode); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		_ = root.Remove(temp)
		return err
	}
	return nil
}

func writeFile(_ context.Context, c *call, args arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	name, err := c.s.confine(app.Dir, args.str("path"))
	if err == nil {
		err = writeAppFile(app.Dir, name, []byte(args.str("content")))
	}
	c.s.ops.Audit(c.actor, c.app, "vibe-write", name, err)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.str("content")), name), nil
}

func editFile(_ context.Context, c *call, args arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	name, err := c.s.confine(app.Dir, args.str("path"))
	if err != nil {
		return "", err
	}
	old, replacement := args.str("old_string"), args.str("new_string")
	if old == "" {
		return "", fault.Invalidf("old_string is empty; use write_file to create a file")
	}
	root, err := os.OpenRoot(app.Dir)
	if err != nil {
		return "", err
	}
	data, err := root.ReadFile(name)
	root.Close()
	if err != nil {
		return "", err
	}
	count := strings.Count(string(data), old)
	switch {
	case count == 0:
		return "", fault.Invalidf("old_string was not found in %s", name)
	case count > 1 && !args.flag("replace_all"):
		return "", fault.Invalidf("old_string matches %d times in %s; add surrounding lines or set replace_all", count, name)
	}
	updated := strings.Replace(string(data), old, replacement, map[bool]int{true: -1, false: 1}[args.flag("replace_all")])
	err = writeAppFile(app.Dir, name, []byte(updated))
	c.s.ops.Audit(c.actor, c.app, "vibe-edit", name, err)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", map[bool]int{true: count, false: 1}[args.flag("replace_all")], name), nil
}

func search(ctx context.Context, c *call, args arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	pattern := args.str("pattern")
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return "", fault.Invalidf("invalid regular expression: %v", err)
	}
	scope := "."
	if args.str("path") != "" {
		if scope, err = c.s.confine(app.Dir, args.str("path")); err != nil {
			return "", err
		}
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, rg, "--line-number", "--no-heading", "--color=never", "--max-columns=300", "--max-count=50", "-e", pattern, "--", scope)
		cmd.Dir = app.Dir
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			return "", fmt.Errorf("search: %w", err)
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return "no matches", nil
		}
		return hitList(lines), nil
	}
	files, err := c.s.appFiles(app.Dir)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, name := range files {
		if scope != "." && name != filepath.ToSlash(scope) && !strings.HasPrefix(name, filepath.ToSlash(scope)+"/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(app.Dir, name))
		if err != nil || len(data) > maxReadBytes || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue
		}
		for number, line := range strings.Split(string(data), "\n") {
			if expression.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", name, number+1, firstLine(line, 300)))
			}
		}
		if len(hits) > maxSearchHits {
			break
		}
	}
	if len(hits) == 0 {
		return "no matches", nil
	}
	return hitList(hits), nil
}

func hitList(hits []string) string {
	text := strings.Join(hits[:min(len(hits), maxSearchHits)], "\n")
	if len(hits) > maxSearchHits {
		text += fmt.Sprintf("\n[... %d more matches; narrow the pattern or path]", len(hits)-maxSearchHits)
	}
	return text
}

func run(_ context.Context, c *call, args arguments) (string, error) {
	timeout := time.Duration(min(max(args.num("timeout_seconds", 60), 1), int(maxRunTimeout/time.Second))) * time.Second
	result, err := c.s.ops.Do(ops.Request{Method: ops.ActionExec, App: c.app, Argv: []string{"/bin/sh", "-c", args.str("command")}, Timeout: timeout, Actor: c.actor})
	if err != nil {
		return "", err
	}
	exec := result.(supervisor.ExecResult)
	output := exec.Output
	if len(output) > maxResult {
		output = fmt.Sprintf("[... first %d characters cut]\n", len(output)-maxResult) + output[len(output)-maxResult:]
	}
	return fmt.Sprintf("exit code %d\n%s", exec.ExitCode, output), nil
}

func restart(_ context.Context, c *call, args arguments) (string, error) {
	if _, err := c.s.ops.Do(ops.Request{Method: ops.ActionRestart, App: c.app, Process: args.str("process"), Actor: c.actor}); err != nil {
		return "", err
	}
	c.restarted = true
	app, err := c.snapshot()
	if err != nil {
		return "restarted", nil
	}
	return "restarted; the app is " + string(app.State), nil
}

func logs(_ context.Context, c *call, args arguments) (string, error) {
	limit := min(max(args.num("lines", 80), 1), 500)
	rows, err := c.s.ops.SearchLogs(c.app, logstore.LogFilter{Process: args.str("process"), Query: args.str("query"), Level: args.str("level"), Limit: limit})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "no log rows", nil
	}
	var b strings.Builder
	for index := len(rows) - 1; index >= 0; index-- {
		row := rows[index]
		fmt.Fprintf(&b, "%s %s %s: %s\n", row.Time.Local().Format("15:04:05"), row.Level, row.Process, firstLine(row.Raw, 1000))
	}
	return b.String(), nil
}

func exceptions(_ context.Context, c *call, _ arguments) (string, error) {
	groups, err := c.s.ops.Exceptions(c.app, logstore.ExceptionFilter{Since: time.Now().Add(-24 * time.Hour), Limit: 20})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, group := range groups {
		if group.IsResolved {
			continue
		}
		message := ""
		if len(group.Minutes) > 0 {
			message = group.Minutes[0].Message
		}
		fmt.Fprintf(&b, "## %s: %d times, last %s\n%s\n", group.ExpUID, group.Count, group.LastAt.Local().Format(time.DateTime), message)
		if group.Dump != "" {
			fmt.Fprintf(&b, "%s\n", capText(group.Dump, 3000))
		}
	}
	if b.Len() == 0 {
		return "no unresolved exceptions in the last day", nil
	}
	return b.String(), nil
}

func gitStatus(ctx context.Context, c *call, _ arguments) (string, error) {
	app, err := c.snapshot()
	if err != nil {
		return "", err
	}
	status, err := git.Status(ctx, app.Dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "branch %s", status.Branch)
	if status.Upstream != "" {
		fmt.Fprintf(&b, ", %d ahead of %s", status.Ahead, status.Upstream)
	}
	b.WriteString("\n")
	if len(status.Changes) == 0 {
		b.WriteString("no uncommitted changes\n")
	}
	for _, change := range status.Changes {
		fmt.Fprintf(&b, "%s %s (+%d -%d)\n", change.Status, change.Path, change.Additions, change.Deletions)
	}
	for _, commit := range status.Unpushed {
		fmt.Fprintf(&b, "unpushed %s %s\n", commit.Short, commit.Subject)
	}
	return b.String(), nil
}

func commit(_ context.Context, c *call, args arguments) (string, error) {
	result, err := c.s.ops.Do(ops.Request{Method: ops.ActionGitCommit, App: c.app, Message: args.str("message"), Actor: c.actor})
	if err != nil {
		return "", err
	}
	return "committed " + result.(ops.GitCommitResult).Hash, nil
}
