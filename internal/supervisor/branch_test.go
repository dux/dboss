package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
	"dboss/internal/ports"
)

// branchCheckout turns the demo app folder into its own repository with main and feature
// branches, leaving it on feature.
func branchCheckout(t *testing.T, cfg config.Config) (string, func(...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	checkout := filepath.Join(cfg.Apps, "demo")
	git := func(args ...string) string {
		t.Helper()
		args = append([]string{"-C", checkout, "-c", "user.name=test", "-c", "user.email=t@t"}, args...)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "ignored.txt\n")
	write("tracked.txt", "main\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "Initial app")
	git("checkout", "-q", "-b", "feature")
	return checkout, git
}

func TestStartStashesAndSwitchesToConfiguredBranch(t *testing.T) {
	cfg := hookConfig(t, [2]int{33960, 33980}, "procfile:\n  worker: /bin/sleep 30\nautostart: false\nbranch: main\n")
	checkout, git := branchCheckout(t, cfg)
	for name, body := range map[string]string{"tracked.txt": "local edit\n", "untracked.txt": "new\n", "ignored.txt": "secret\n"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if snapshot, _ := manager.Snapshot("demo"); snapshot.Branch != "feature" || snapshot.RequiredBranch != "main" {
		t.Fatalf("before start: branch %q, required %q", snapshot.Branch, snapshot.RequiredBranch)
	}
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if branch := git("symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Fatalf("checkout is on %q", branch)
	}
	if stash := git("stash", "list"); !strings.Contains(stash, "dboss: before switching to main") {
		t.Fatalf("stash = %q", stash)
	}
	if status := git("status", "--porcelain"); status != "" {
		t.Fatalf("local changes left behind: %q", status)
	}
	if data, err := os.ReadFile(filepath.Join(checkout, "ignored.txt")); err != nil || string(data) != "secret\n" {
		t.Fatalf("ignored file = %q, %v", data, err)
	}
	// The switch rescans, so the card shows the branch the app now runs.
	if snapshot, _ := manager.Snapshot("demo"); snapshot.Branch != "main" {
		t.Fatalf("after start: branch %q", snapshot.Branch)
	}

	// Already on the branch, a start leaves local changes alone.
	if err := manager.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "tracked.txt"), []byte("edit on main\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Running)
	if stash := git("stash", "list"); strings.Count(stash, "\n") != 0 {
		t.Fatalf("second start stashed again: %q", stash)
	}
	if status := git("status", "--porcelain"); status != "M tracked.txt" {
		t.Fatalf("status = %q", status)
	}
}

func TestStartFailsWhenBranchCannotBeCheckedOut(t *testing.T) {
	cfg := hookConfig(t, [2]int{33980, 34000}, "procfile:\n  worker: /bin/sleep 30\nautostart: false\nbranch: release\n")
	_, git := branchCheckout(t, cfg)
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start("demo"); err != nil {
		t.Fatal(err)
	}
	waitForSupervisorState(t, manager, Crashed)
	snapshot, _ := manager.Snapshot("demo")
	if !strings.Contains(snapshot.Error, "lifecycle branch") || len(snapshot.Processes) != 1 || snapshot.Processes[0].PID != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	log, err := os.ReadFile(filepath.Join(cfg.LogDir, "demo", "lifecycle-branch.log"))
	if err != nil || !strings.Contains(string(log), "cannot check out release") {
		t.Fatalf("branch log = %q, %v", log, err)
	}
	if branch := git("symbolic-ref", "--short", "HEAD"); branch != "feature" {
		t.Fatalf("checkout moved to %q", branch)
	}
}

func TestPullDeployRefusesAnotherBranch(t *testing.T) {
	cfg := hookConfig(t, [2]int{34000, 34020}, "procfile:\n  worker: /bin/sleep 30\nautostart: false\nbranch: main\nhooks:\n  deploy: true\n")
	branchCheckout(t, cfg)
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	if state := waitForHookEnd(t, manager, "demo", "deploy"); state.LastExit == 0 {
		t.Fatalf("deploy on feature = %+v", state)
	}
	infos, err := manager.Hooks("demo")
	if err != nil || len(infos) != 1 || !strings.Contains(infos[0].Output, "not on branch main, refusing to deploy") {
		t.Fatalf("deploy output = %+v, %v", infos, err)
	}
}
