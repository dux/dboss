// Package children records every process a session starts, so the next session can stop what an
// unclean exit (kill -9, a crash, a closed terminal) left running. Every child runs in its own
// session, so nothing else takes it down with dboss, and a worker holds no port a later start
// would clear.
package children

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dboss/internal/fsutil"
)

// errGone is what startTime reports for a pid with no live process.
var errGone = errors.New("no such process")

// Ledger is one file per live child under <state dir>/children, named by pid and holding the
// process start time (which tells the child apart from a later process that reused its pid) and a
// label for the log.
type Ledger struct{ dir string }

func New(stateDir string) Ledger { return Ledger{dir: filepath.Join(stateDir, "children")} }

// Start starts cmd and records it. A child that cannot be recorded is killed, so the session never
// runs a process the next one could not find.
func (l Ledger) Start(cmd *exec.Cmd, label string) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	err := os.MkdirAll(l.dir, 0o750)
	if err == nil {
		var start string
		if start, err = startTime(pid); err == nil || errors.Is(err, errors.ErrUnsupported) {
			err = fsutil.WriteFile(l.path(pid), []byte(start+"\n"+label+"\n"), 0o640)
		}
	}
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return fmt.Errorf("record %s: %w", label, err)
	}
	return nil
}

// Done forgets a child once Wait returned.
func (l Ledger) Done(pid int) { _ = os.Remove(l.path(pid)) }

func (l Ledger) path(pid int) string { return filepath.Join(l.dir, strconv.Itoa(pid)) }

// Reap stops the process group of every child an earlier session recorded and returns their
// labels. It must only run while no other session uses the same state dir. A pid that now
// belongs to another process is only forgotten.
func (l Ledger) Reap(timeout time.Duration) ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var labels []string
	var groups []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		data, err := os.ReadFile(l.path(pid))
		if err != nil {
			return labels, err
		}
		recorded, label, _ := strings.Cut(strings.TrimSuffix(string(data), "\n"), "\n")
		if orphaned(pid, recorded) {
			if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return labels, fmt.Errorf("stop %s (pid %d): %w", label, pid, err)
			}
			groups = append(groups, pid)
			labels = append(labels, fmt.Sprintf("%s (pid %d)", label, pid))
		}
		l.Done(pid)
	}
	deadline := time.Now().Add(timeout)
	for len(groups) > 0 {
		groups = alive(groups)
		if len(groups) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range groups {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return labels, nil
}

// orphaned reports whether the group led by pid is still the recorded child's. While the leader
// lives its start time must match. Once it is gone a surviving group can only be the old one: the
// kernel never hands out a pid that is still a live group's id.
func orphaned(pid int, recorded string) bool {
	start, err := startTime(pid)
	switch {
	case err == nil:
		return recorded != "" && start == recorded
	case errors.Is(err, errGone):
		return syscall.Kill(-pid, 0) == nil
	default:
		return false
	}
}

func alive(groups []int) []int {
	var left []int
	for _, pid := range groups {
		if syscall.Kill(-pid, 0) == nil {
			left = append(left, pid)
		}
	}
	return left
}
