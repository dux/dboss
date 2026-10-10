package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dboss/internal/fault"
)

const (
	worktreeTimeout = 30 * time.Second
	pushTimeout     = 2 * time.Minute
	// MaxDiff caps one file's diff; a bigger one is reported as too large instead of sent.
	MaxDiff = 200 << 10
	// maxUnpushed bounds the unpushed commits listed with their files.
	maxUnpushed = 30
	// maxCountBytes bounds how much of an untracked file is read to count its lines.
	maxCountBytes = 1 << 20
)

// Change is one file that differs from HEAD, or one file a commit touched. Status is M, A, D, R
// or ? (untracked).
type Change struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

// LogCommit is one commit ahead of the upstream, with the files it touched.
type LogCommit struct {
	Hash    string    `json:"hash"`
	Short   string    `json:"short"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Time    time.Time `json:"time"`
	Files   []Change  `json:"files"`
}

// Worktree is what Commit would take and what Push would send.
type Worktree struct {
	Branch   string      `json:"branch"`
	Upstream string      `json:"upstream,omitempty"`
	Ahead    int         `json:"ahead"`
	Behind   int         `json:"behind"`
	Changes  []Change    `json:"changes"`
	Unpushed []LogCommit `json:"unpushed"`
}

// command runs git in dir and returns stdout. okCodes lists exit codes that are not failures, for
// `git diff --no-index`, which answers 1 when the files differ.
func command(ctx context.Context, dir string, env []string, okCodes []int, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			for _, code := range okCodes {
				if exit.ExitCode() == code {
					return out.String(), nil
				}
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func localEnv() []string { return append(commandEnv(""), "GIT_OPTIONAL_LOCKS=0") }

// ownRepo refuses a folder that is not the root of its own checkout: inside a larger repository,
// status, commit, push and reset would act on files that belong to other projects.
func ownRepo(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return fault.Invalidf("%s is not the root of its own git repository", filepath.Base(dir))
	}
	return nil
}

// Status reads the working tree against HEAD and the commits ahead of the upstream. Untracked
// files that are not ignored count as changes.
func Status(ctx context.Context, dir string) (Worktree, error) {
	if err := ownRepo(dir); err != nil {
		return Worktree{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	env := localEnv()
	out, err := command(ctx, dir, env, nil, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return Worktree{}, err
	}
	result := Worktree{Changes: []Change{}, Unpushed: []LogCommit{}}
	records := strings.Split(out, "\x00")
	for index := 0; index < len(records); index++ {
		record := records[index]
		switch {
		case strings.HasPrefix(record, "# branch.head "):
			result.Branch = strings.TrimPrefix(record, "# branch.head ")
		case strings.HasPrefix(record, "# branch.upstream "):
			result.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
		case strings.HasPrefix(record, "# branch.ab "):
			fmt.Sscanf(strings.TrimPrefix(record, "# branch.ab "), "+%d -%d", &result.Ahead, &result.Behind)
		case strings.HasPrefix(record, "1 "), strings.HasPrefix(record, "u "):
			fields := strings.SplitN(record, " ", 9)
			if strings.HasPrefix(record, "u ") {
				fields = strings.SplitN(record, " ", 11)
			}
			result.Changes = append(result.Changes, Change{Path: fields[len(fields)-1], Status: changeStatus(fields[1])})
		case strings.HasPrefix(record, "2 "):
			fields := strings.SplitN(record, " ", 10)
			change := Change{Path: fields[9], Status: "R"}
			if index+1 < len(records) {
				change.OldPath = records[index+1]
				index++
			}
			result.Changes = append(result.Changes, change)
		case strings.HasPrefix(record, "? "):
			result.Changes = append(result.Changes, Change{Path: strings.TrimPrefix(record, "? "), Status: "?"})
		}
	}
	if result.Branch == "(detached)" {
		result.Branch = ""
	}
	if err := countChanges(ctx, dir, env, result.Changes); err != nil {
		return result, err
	}
	if result.Upstream != "" && result.Ahead > 0 {
		result.Unpushed, err = unpushed(ctx, dir, env)
	}
	return result, err
}

// changeStatus folds porcelain v2's index and work tree letters into one: added and deleted win,
// anything else is modified.
func changeStatus(xy string) string {
	switch {
	case strings.Contains(xy, "A"):
		return "A"
	case strings.Contains(xy, "D"):
		return "D"
	case strings.Contains(xy, "R"):
		return "R"
	}
	return "M"
}

// countChanges fills the line counts: numstat against HEAD for tracked files, a line count for
// untracked ones. An unborn branch has no HEAD to compare with, so its files stay uncounted.
func countChanges(ctx context.Context, dir string, env []string, changes []Change) error {
	byPath := map[string]*Change{}
	for index := range changes {
		change := &changes[index]
		if change.Status == "?" {
			change.Additions, change.Binary = countLines(filepath.Join(dir, change.Path))
			continue
		}
		byPath[change.Path] = change
	}
	if len(byPath) == 0 {
		return nil
	}
	if _, err := command(ctx, dir, env, nil, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return nil
	}
	out, err := command(ctx, dir, env, nil, "diff", "--numstat", "-z", "-M", "HEAD")
	if err != nil {
		return err
	}
	for _, stat := range parseNumstat(out) {
		if change, ok := byPath[stat.Path]; ok {
			change.Additions, change.Deletions, change.Binary = stat.Additions, stat.Deletions, stat.Binary
		}
	}
	return nil
}

// parseNumstat reads `--numstat -z`: "add\tdel\tpath\0", or "add\tdel\t\0old\0new\0" for a
// rename; a binary file has "-" for both counts.
func parseNumstat(out string) []Change {
	var changes []Change
	records := strings.Split(out, "\x00")
	for index := 0; index < len(records); index++ {
		parts := strings.SplitN(records[index], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		change := Change{Path: parts[2], Status: "M"}
		if parts[0] == "-" && parts[1] == "-" {
			change.Binary = true
		} else {
			change.Additions, _ = strconv.Atoi(parts[0])
			change.Deletions, _ = strconv.Atoi(parts[1])
		}
		if change.Path == "" && index+2 < len(records) {
			change.OldPath, change.Path, change.Status = records[index+1], records[index+2], "R"
			index += 2
		}
		changes = append(changes, change)
	}
	return changes
}

func countLines(path string) (int, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	buffer := make([]byte, maxCountBytes)
	n, _ := file.Read(buffer)
	data := buffer[:n]
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, true
	}
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines, false
}

// unpushed lists the commits ahead of the upstream, newest first, each with the files it touched.
func unpushed(ctx context.Context, dir string, env []string) ([]LogCommit, error) {
	out, err := command(ctx, dir, env, nil, "log", "--max-count="+strconv.Itoa(maxUnpushed), "--format=%H%x1f%h%x1f%s%x1f%an%x1f%aI%x1e", "@{upstream}..HEAD", "--")
	if err != nil {
		return nil, err
	}
	commits := []LogCommit{}
	for _, record := range strings.Split(out, "\x1e") {
		fields := strings.Split(strings.TrimSpace(record), "\x1f")
		if len(fields) != 5 {
			continue
		}
		commit := LogCommit{Hash: fields[0], Short: fields[1], Subject: fields[2], Author: fields[3], Files: []Change{}}
		commit.Time, _ = time.Parse(time.RFC3339, fields[4])
		stats, err := command(ctx, dir, env, nil, "diff-tree", "--no-commit-id", "--root", "-r", "-M", "--numstat", "-z", commit.Hash)
		if err != nil {
			return nil, err
		}
		names, err := command(ctx, dir, env, nil, "diff-tree", "--no-commit-id", "--root", "-r", "-M", "--name-status", "-z", commit.Hash)
		if err != nil {
			return nil, err
		}
		statuses := parseNameStatus(names)
		for _, change := range parseNumstat(stats) {
			if status, ok := statuses[change.Path]; ok {
				change.Status = status
			}
			commit.Files = append(commit.Files, change)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

// parseNameStatus maps each path of `--name-status -z` to its letter; a rename names its new path.
func parseNameStatus(out string) map[string]string {
	statuses := map[string]string{}
	records := strings.Split(out, "\x00")
	for index := 0; index+1 < len(records); index += 2 {
		letter := records[index]
		if letter == "" {
			continue
		}
		path := records[index+1]
		if letter[0] == 'R' || letter[0] == 'C' {
			if index+2 < len(records) {
				path = records[index+2]
			}
			index++
		}
		statuses[path] = changeStatus(letter[:1])
	}
	return statuses
}

// Diff is one file's working tree diff against HEAD; an untracked file diffs against nothing. A
// diff over MaxDiff fails with fault.Invalid, so the caller says so instead of sending it.
func Diff(ctx context.Context, dir string, change Change) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	env := localEnv()
	var out string
	var err error
	switch {
	case change.Status == "?":
		out, err = command(ctx, dir, env, []int{1}, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", os.DevNull, change.Path)
	case change.OldPath != "":
		out, err = command(ctx, dir, env, nil, "diff", "--no-color", "--no-ext-diff", "-M", "HEAD", "--", change.OldPath, change.Path)
	default:
		out, err = command(ctx, dir, env, nil, "diff", "--no-color", "--no-ext-diff", "HEAD", "--", change.Path)
	}
	return capDiff(out, err)
}

// CommitDiff is one file's change in one commit.
func CommitDiff(ctx context.Context, dir, commit string, change Change) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	paths := []string{change.Path}
	if change.OldPath != "" {
		paths = []string{change.OldPath, change.Path}
	}
	out, err := command(ctx, dir, localEnv(), nil, append([]string{"show", "--no-color", "--no-ext-diff", "-M", "--format=", commit, "--"}, paths...)...)
	return capDiff(out, err)
}

func capDiff(out string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if len(out) > MaxDiff {
		return "", fault.Invalidf("the diff is %d KB, too large to show", len(out)>>10)
	}
	return out, nil
}

// CommitAll stages every change, untracked files included and ignored ones not, and commits it.
// A checkout with no git identity commits as author. It returns the new commit's short hash.
func CommitAll(ctx context.Context, dir, message, author string) (string, error) {
	if err := ownRepo(dir); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	if strings.TrimSpace(message) == "" {
		return "", fault.Invalidf("a commit message is required")
	}
	env := commandEnv("")
	if status, err := command(ctx, dir, env, nil, "status", "--porcelain", "--untracked-files=all"); err != nil {
		return "", err
	} else if strings.TrimSpace(status) == "" {
		return "", fault.Invalidf("nothing to commit")
	}
	if _, err := command(ctx, dir, env, nil, "add", "--all"); err != nil {
		return "", err
	}
	args := append(identity(ctx, dir, env, author), "commit", "--quiet", "--no-verify", "--file=-")
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(message)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	hash, err := command(ctx, dir, env, nil, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), err
}

// identity is the -c pair that lends author ("Name <email>") to a checkout with no git identity of
// its own; a configured identity always wins.
func identity(ctx context.Context, dir string, env []string, author string) []string {
	if email, _ := command(ctx, dir, env, []int{1}, "config", "user.email"); strings.TrimSpace(email) != "" {
		return nil
	}
	name, rest, ok := strings.Cut(author, "<")
	if !ok {
		name, rest = "dboss", "dboss@localhost"
	}
	return []string{"-c", "user.name=" + strings.TrimSpace(name), "-c", "user.email=" + strings.TrimSuffix(strings.TrimSpace(rest), ">")}
}

// Push pushes the current branch to its upstream. A nonempty required branch refuses a checkout
// on any other, like a pull deploy does. It returns git's own output.
func Push(ctx context.Context, dir, token, required string) (string, error) {
	if err := ownRepo(dir); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	env := commandEnv(token)
	branch, err := command(ctx, dir, env, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fault.Invalidf("push needs a checkout on a branch")
	}
	branch = strings.TrimSpace(branch)
	if required != "" && branch != required {
		return "", fault.Invalidf("checkout is on branch %q, the app requires %q", branch, required)
	}
	if _, err := command(ctx, dir, env, nil, "rev-parse", "--symbolic-full-name", "@{upstream}"); err != nil {
		return "", fault.Invalidf("branch %q has no upstream to push to", branch)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "push", "--porcelain")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Reset clears every uncommitted change, untracked files included and ignored ones not, by
// stashing it, so `git stash pop` brings the work back. It returns the stash's name.
func Reset(ctx context.Context, dir, message, author string) (string, error) {
	if err := ownRepo(dir); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	defer cancel()
	env := commandEnv("")
	if status, err := command(ctx, dir, env, nil, "status", "--porcelain", "--untracked-files=all"); err != nil {
		return "", err
	} else if strings.TrimSpace(status) == "" {
		return "", fault.Invalidf("nothing to reset")
	}
	args := append(identity(ctx, dir, env, author), "stash", "push", "--include-untracked", "--message", message)
	if _, err := command(ctx, dir, env, nil, args...); err != nil {
		return "", err
	}
	return "stash@{0}", nil
}
