package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"dboss/internal/schedule"

	"gopkg.in/yaml.v3"
)

// jobName is the shape of every name the app file declares: processes, cron jobs and hooks.
var jobName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidAppName checks the folder name of an app dboss creates itself. Existing folders are not
// held to it; it keeps a created name usable in URLs, cgroup paths and log databases.
func ValidAppName(name string) error {
	if !jobName.MatchString(name) {
		return fmt.Errorf("invalid app name %q: use lowercase letters, digits, - and _, starting with a letter", name)
	}
	return nil
}

func validateCron(jobs map[string]CronJob) error {
	for name, job := range jobs {
		if !jobName.MatchString(name) {
			return keyErr("cron", "invalid job name %q", name)
		}
		if strings.TrimSpace(job.Command) == "" {
			return keyErr("cron."+name+".command", "must not be empty")
		}
		if _, err := schedule.Parse(job.Schedule); err != nil {
			return &Error{Key: "cron." + name + ".schedule", Message: err.Error(), Hint: "use every 5m, every 2h, every 1d or a 5-field cron expression"}
		}
		if job.Timeout < 0 {
			return keyErr("cron."+name+".timeout", "cannot be negative")
		}
	}
	return nil
}

// LifecycleSteps are the lifecycle entries an app may declare, in the order they can run.
var LifecycleSteps = []string{"create", "start", "destroy"}

func validateLifecycle(lifecycle map[string]LifecycleCommand) error {
	for name, step := range lifecycle {
		if !slices.Contains(LifecycleSteps, name) {
			return &Error{Key: "lifecycle", Message: fmt.Sprintf("unknown step %q", name), Hint: "valid steps: " + strings.Join(LifecycleSteps, ", ")}
		}
		if strings.TrimSpace(step.Command) == "" {
			return keyErr("lifecycle."+name+".command", "must not be empty")
		}
		if step.Timeout < 0 {
			return keyErr("lifecycle."+name+".timeout", "cannot be negative")
		}
	}
	return nil
}

func validateHooks(hooks map[string]Hook) error {
	for name, hook := range hooks {
		if !jobName.MatchString(name) {
			return keyErr("hooks", "invalid hook name %q", name)
		}
		if hasBuiltinFields(hook) {
			return keyErr("hooks."+name, "uses github_pr options; those are only valid on the host github_pr hook")
		}
		if strings.TrimSpace(hook.Command) == "" {
			return keyErr("hooks."+name+".command", "must not be empty")
		}
		if hook.Timeout < 0 {
			return keyErr("hooks."+name+".timeout", "cannot be negative")
		}
	}
	return nil
}

// BuiltinGithubPR is the reserved host hook name that deploys a branch as an app from a template.
const BuiltinGithubPR = "github_pr"

// hasBuiltinFields reports whether a hook uses options that only the github_pr built-in reads.
func hasBuiltinFields(hook Hook) bool {
	return hook.Repo != "" || hook.Template != nil
}

// validateHostHooks checks the host-level hooks block: only the github_pr built-in is supported
// there for now, and it must carry a template with a name.
func validateHostHooks(hooks map[string]Hook) error {
	for name, hook := range hooks {
		if !jobName.MatchString(name) {
			return keyErr("hooks", "invalid hook name %q", name)
		}
		if name != BuiltinGithubPR {
			return keyErr("hooks."+name, "host hooks support only the built-in %q", BuiltinGithubPR)
		}
		if strings.TrimSpace(hook.Command) != "" {
			return keyErr("hooks."+name, "the built-in %q takes no command; it deploys and restarts itself", BuiltinGithubPR)
		}
		if len(hook.Template) == 0 {
			return keyErr("hooks."+name+".template", "must define the app the preview runs")
		}
		if _, ok := hook.Template["name"]; !ok {
			return keyErr("hooks."+name+".template.name", "is required")
		}
	}
	return nil
}

// CronJob is one named scheduled one-shot command under cron:. Schedule is an "every <n><s|m|h|d>"
// interval or a five-field cron expression. Overlap false skips a run while the previous one is
// still going; a zero Timeout means no limit.
type CronJob struct {
	Schedule string   `yaml:"schedule" json:"schedule"`
	Command  string   `yaml:"command" json:"command"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Overlap  bool     `yaml:"overlap" json:"overlap"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
}

// pullCommand is what a scalar `hook: true` runs: fast-forward the branch checked out in the
// repository that contains the app. The existing restart path rolls the app when git exits 0.
const pullCommand = "git pull --ff-only"

// Hook is one named one-shot command triggered by a ping to /hooks/<app>/<hook> signed with
// tokens.dboss. Restart restarts the app when the command exits 0. A scalar true is shorthand
// for {command: git pull --ff-only, restart: true}.
type Hook struct {
	Command  string   `yaml:"command" json:"command"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Restart  bool     `yaml:"restart" json:"restart"`
	Overlap  bool     `yaml:"overlap" json:"overlap"`
	Disabled bool     `yaml:"disabled" json:"disabled"`
	// Pull marks the scalar shorthand; the pull job then authenticates with tokens.github.
	Pull bool `yaml:"-" json:"pull,omitempty"`

	// The fields below configure the built-in github_pr host hook. They are invalid on an app
	// hook and on any other host hook name.
	Repo     string         `yaml:"repo" json:"repo,omitempty"`
	Template map[string]any `yaml:"template" json:"template,omitempty"`
}

// PullHook fast-forwards the current branch and restarts after a successful pull.
func PullHook() Hook {
	return Hook{Command: pullCommand, Restart: true, Pull: true}
}

// branchName keeps `branch` to plain ref characters, so it can be quoted into the shell lines
// below as it is.
var branchName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

func validateBranch(branch string) error {
	if branch == "" {
		return nil
	}
	if !branchName.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "/.") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") {
		return keyErr("branch", "invalid git branch name %q", branch)
	}
	return nil
}

// PullCommand is what a pull hook runs. With a branch it refuses a checkout on any other branch
// instead of pulling into it.
func PullCommand(branch string) string {
	if branch == "" {
		return pullCommand
	}
	return fmt.Sprintf(`[ "$(git symbolic-ref --quiet --short HEAD)" = '%[1]s' ] || { echo "dboss: checkout is not on branch %[1]s, refusing to deploy" >&2; exit 1; }; %[2]s`, branch, pullCommand)
}

// BranchCommand is the start step that puts the checkout on branch. On any other branch it
// stashes local changes and untracked files (ignored files stay) and checks branch out; when
// either fails the step exits non-zero and the start fails with git's output in its log.
func BranchCommand(branch string) string {
	return fmt.Sprintf(`current=$(git symbolic-ref --quiet --short HEAD)
[ "$current" = '%[1]s' ] && exit 0
echo "dboss: checkout is on ${current:-a detached HEAD}, switching to %[1]s"
git stash push --include-untracked --message 'dboss: before switching to %[1]s' || { echo "dboss: cannot stash local changes" >&2; exit 1; }
git checkout '%[1]s' || { echo "dboss: cannot check out %[1]s" >&2; exit 1; }`, branch)
}

// LifecycleCommand is one lifecycle step: create runs once before the app's first start, start
// before every start, destroy after the app is stopped and detached, before its folder goes.
// A scalar is the command alone; a zero Timeout means DefaultLifecycleTimeout.
type LifecycleCommand struct {
	Command string   `yaml:"command" json:"command"`
	Timeout Duration `yaml:"timeout" json:"timeout,omitempty"`
}

// DefaultLifecycleTimeout bounds a lifecycle step that sets no timeout.
const DefaultLifecycleTimeout = 3 * time.Minute

// UnmarshalYAML accepts a bare command or a {command, timeout} mapping.
func (l *LifecycleCommand) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		l.Command = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			switch key := node.Content[i].Value; key {
			case "command", "timeout":
			default:
				return &Error{Line: node.Content[i].Line, Key: "lifecycle", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: command, timeout"}
			}
		}
		type plain LifecycleCommand
		return node.Decode((*plain)(l))
	}
	return &Error{Line: node.Line, Key: "lifecycle", Message: "must be a command or a {command, timeout} mapping"}
}

// UnmarshalYAML accepts a bare true, shorthand for pulling the current branch and restarting, or
// a {command, timeout, restart, overlap, disabled} mapping. The keys are checked here
// because a custom decoder is a leaf as far as the schema walk is concerned.
func (h *Hook) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!bool" || node.Value != "true" {
			return &Error{Line: node.Line, Key: "hooks", Message: "must be true or a mapping", Hint: "delete the hook or write disabled: true"}
		}
		*h = PullHook()
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			switch key := node.Content[i].Value; key {
			case "command", "timeout", "restart", "overlap", "disabled",
				"repo", "template":
			case "secret":
				return &Error{Line: node.Content[i].Line, Key: "hooks", Message: "secret was removed", Hint: "every hook is signed with tokens.dboss in dboss-server.yaml"}
			default:
				return &Error{Line: node.Content[i].Line, Key: "hooks", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: command, timeout, restart, overlap, disabled"}
			}
		}
		type plain Hook
		return node.Decode((*plain)(h))
	}
	return &Error{Line: node.Line, Key: "hooks", Message: "must be true or a {command, timeout, restart, overlap, disabled} mapping"}
}
