package supervisor

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/logx"
	"dboss/internal/sandbox"
)

// newCommand builds one child of an app: a config line through /bin/sh -c, or the argv of
// `dboss exec` (already split by the caller's shell) with argv[0] resolved against the app's
// PATH. It runs in dir, in its own session, so a stop signals the shell and its children alike.
// With a policy it starts through the sandbox launcher instead.
func newCommand(dir string, command apps.Command, env map[string]string, policy *sandbox.Policy) (*exec.Cmd, error) {
	argv := []string{"/bin/sh", "-c", command.Line}
	if command.Line == "" {
		resolved, err := resolveExecutable(command.Argv[0], dir, env["PATH"])
		if err != nil {
			return nil, err
		}
		argv = append([]string{resolved}, command.Argv[1:]...)
	}
	var cmd *exec.Cmd
	if policy != nil {
		// Both point into the runtime folder, which the sandbox hides.
		delete(env, "DBOSS_SOCKET")
		delete(env, "DBOSS_EVENTS_DIR")
		var err error
		if cmd, err = sandbox.Command(*policy, argv); err != nil {
			return nil, err
		}
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = envSlice(env)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return cmd, nil
}

// sandboxPolicy is the policy one command of the app runs under, or nil when it runs unconfined:
// the app keeps the sandbox off, or the command is dboss's own. A machine that cannot sandbox
// refuses the command on a host, so a process that asked for a sandbox never runs without one; a
// dev session warns and runs it unconfined. port is the process's PORT, 0 for jobs and steps.
func sandboxPolicy(cfg config.Config, spec *apps.App, box config.Sandbox, command apps.Command, port int) (*sandbox.Policy, error) {
	if !box.Enabled || command.Builtin {
		return nil, nil
	}
	if err := sandbox.Check(); err != nil {
		if cfg.Dev() {
			logx.Warnf("%s/%s: the sandbox is off in this dev session: %v", spec.Name, command.Name, err)
			return nil, nil
		}
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	paths := func(list config.List) []string {
		resolved := make([]string, len(list))
		for i, path := range list {
			resolved[i] = resolveDir(spec.Dir, path)
		}
		return resolved
	}
	policy := &sandbox.Policy{
		Dir:     spec.Dir,
		Read:    paths(box.Read),
		Write:   paths(box.Write),
		Deny:    append(paths(box.Deny), cfg.RuntimeDir),
		Sockets: paths(box.Sockets),
		Bind:    append([]int(nil), box.Bind...),
		Connect: box.Connect,
	}
	if cfg.Apps != "" {
		policy.Hide = []string{cfg.Apps}
	}
	if port > 0 {
		policy.Bind = append(policy.Bind, port)
	}
	return policy, nil
}

// exitCode is what Wait reported: 0 on success, the exit status, or -1 when the child did not
// exit on its own (killed by a signal, or never started).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
