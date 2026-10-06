package git

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const compareTimeout = 30 * time.Second

type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// Comparison uses the current branch's upstream, the same target git pull reads.
type Comparison struct {
	Branch   string `json:"branch"`
	Upstream string `json:"upstream"`
	Local    Commit `json:"local"`
	Remote   Commit `json:"remote"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Dirty    bool   `json:"dirty"`
}

// Compare refreshes the remote-tracking ref and counts commits on each side without changing
// the local branch, working tree or FETCH_HEAD. A shallow checkout is deepened for exact counts.
// A nonempty required branch refuses a checkout on any other, before anything is fetched.
func Compare(ctx context.Context, dir, token, required string) (Comparison, error) {
	ctx, cancel := context.WithTimeout(ctx, compareTimeout)
	defer cancel()
	env := append(commandEnv(token), "GIT_OPTIONAL_LOCKS=0")
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		cmd.WaitDelay = time.Second
		var out, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	var result Comparison
	branch, err := run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return result, fmt.Errorf("redeploy comparison needs a Git checkout on a branch: %w", err)
	}
	result.Branch = branch
	if required != "" && branch != required {
		return result, fmt.Errorf("checkout is on branch %q, the app requires %q", branch, required)
	}
	upstream, err := run("rev-parse", "--symbolic-full-name", "@{upstream}")
	if err != nil || !strings.HasPrefix(upstream, "refs/remotes/") {
		return result, fmt.Errorf("branch %q needs a remote upstream to compare before redeploy", branch)
	}
	result.Upstream = strings.TrimPrefix(upstream, "refs/remotes/")
	remote, err := run("config", "--get", "branch."+branch+".remote")
	if err != nil {
		return result, err
	}
	merge, err := run("config", "--get", "branch."+branch+".merge")
	if err != nil {
		return result, err
	}
	shallow, err := run("rev-parse", "--is-shallow-repository")
	if err != nil {
		return result, err
	}
	args := []string{"fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--no-auto-maintenance"}
	if shallow == "true" {
		args = append(args, "--unshallow")
	}
	args = append(args, "--", remote, "+"+merge+":"+upstream)
	if _, err := run(args...); err != nil {
		return result, fmt.Errorf("cannot read the latest remote commit: %w", err)
	}
	if current, err := run("symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || current != branch {
		return result, fmt.Errorf("the app's branch changed during comparison; retry")
	}
	for ref, commit := range map[string]*Commit{"HEAD": &result.Local, upstream: &result.Remote} {
		text, err := run("log", "-1", "--format=%H%n%s", ref, "--")
		if err != nil {
			return result, err
		}
		commit.Hash, commit.Subject, _ = strings.Cut(text, "\n")
	}
	counts, err := run("rev-list", "--left-right", "--count", result.Local.Hash+"..."+result.Remote.Hash, "--")
	if err != nil {
		return result, err
	}
	parts := strings.Fields(counts)
	if len(parts) != 2 {
		return result, fmt.Errorf("unexpected Git commit counts %q", counts)
	}
	if result.Ahead, err = strconv.Atoi(parts[0]); err != nil {
		return result, err
	}
	if result.Behind, err = strconv.Atoi(parts[1]); err != nil {
		return result, err
	}
	status, err := run("status", "--porcelain", "--untracked-files=no")
	result.Dirty = status != ""
	return result, err
}
