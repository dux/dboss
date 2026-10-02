package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/git"
	"dboss/internal/ports"
)

func TestAutomaticDeployPullsAndRestartsWithoutHooksConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := hookConfig(t, [2]int{32940, 32960}, "procfile:\n  worker: /bin/sleep 30\nautostart: false\n")
	checkout := filepath.Join(cfg.Apps, "demo")
	root := t.TempDir()
	remote, author := filepath.Join(root, "remote.git"), filepath.Join(root, "author")
	run := func(dir string, args ...string) {
		t.Helper()
		args = append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=t@t"}, args...)
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(checkout, "init", "-q", "-b", "main")
	run(checkout, "add", config.FileName)
	run(checkout, "commit", "-q", "-m", "Initial app")
	run(root, "clone", "-q", "--bare", checkout, remote)
	run(checkout, "remote", "add", "origin", remote)
	run(checkout, "fetch", "-q", "origin")
	run(checkout, "branch", "--set-upstream-to=origin/main")
	run(root, "clone", "-q", remote, author)
	if err := os.WriteFile(filepath.Join(author, "release.txt"), []byte("new release\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	run(author, "add", "release.txt")
	run(author, "commit", "-q", "-m", "New release")
	run(author, "push", "-q")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	infos, err := manager.Hooks("demo")
	if err != nil || len(infos) != 1 || infos[0].Command != "git pull --ff-only" || !infos[0].Restart || infos[0].Disabled {
		t.Fatalf("automatic deploy = %+v, %v", infos, err)
	}
	preview, err := git.Compare(context.Background(), checkout, "")
	if err != nil || preview.Behind != 1 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	state := waitForHookEnd(t, manager, "demo", "deploy")
	if state.LastExit != 0 || state.LastError != "" {
		t.Fatalf("automatic deploy failed: %+v", state)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := manager.Snapshot("demo")
		if len(snapshot.Processes) == 1 && snapshot.Processes[0].PID != 0 && !snapshot.Hooks[0].Restarting {
			data, err := os.ReadFile(filepath.Join(checkout, "release.txt"))
			if err != nil || string(data) != "new release\n" {
				t.Fatalf("checkout did not update: %s, %v", data, err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("automatic deploy did not restart the app")
}

// hookConfig writes a full app config so a test can pick its own procfile command.
func hookConfig(t *testing.T, portRange [2]int, appYAML string) config.Config {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, config.FileName), []byte(appYAML), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Apps = filepath.Join(root, "apps")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "log")
	cfg.Socket = filepath.Join(root, "dboss.sock")
	cfg.Tokens.Dboss = "hook-token"
	cfg.Management.Host = config.List{"dboss.example.com"}
	cfg.Ports = portRange
	cfg.Defaults.StopTimeout = config.Duration(2 * time.Second)
	cfg.Defaults.HealthTimeout = config.Duration(2 * time.Second)
	return cfg
}

func waitForHookEnd(t *testing.T, manager *Manager, app, hook string) HookSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := manager.Snapshot(app)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range snapshot.Hooks {
			if state.Name == hook && !state.Running && !state.LastEnd.IsZero() {
				return state
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hook %s did not finish", hook)
	return HookSnapshot{}
}

func TestHookListsRunsAndCarriesTheToken(t *testing.T) {
	cfg := hookConfig(t, [2]int{32800, 32820}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hello\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	snapshot, err := manager.Snapshot("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Hooks) != 1 || snapshot.Hooks[0].Name != "deploy" {
		t.Fatalf("hook snapshot = %+v", snapshot.Hooks)
	}

	infos, err := manager.Hooks("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].URL != "https://dboss.example.com/hooks/demo/deploy?token=hook-token" {
		t.Fatalf("hook info = %+v", infos)
	}
	token, err := manager.HookToken("demo", "deploy")
	if err != nil || token != "hook-token" {
		t.Fatalf("HookToken = %q, %v", token, err)
	}
	if _, err := manager.HookToken("demo", "missing"); err == nil {
		t.Fatal("an unknown hook must not have a token")
	}

	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	state := waitForHookEnd(t, manager, "demo", "deploy")
	if state.LastExit != 0 || state.LastError != "" {
		t.Fatalf("hook ended badly: %+v", state)
	}
}

func TestHookURLNeedsTheToken(t *testing.T) {
	cfg := hookConfig(t, [2]int{32820, 32840}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo hi\n")
	cfg.Tokens.Dboss = ""
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	infos, _ := manager.Hooks("demo")
	if len(infos) != 1 || infos[0].URL != "" {
		t.Fatalf("a hook without tokens.dboss must have no URL: %+v", infos)
	}
}

func TestHookRefusesAnotherRunDuringItsRestart(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		state := &jobState{kind: "hook", name: "deploy", restarting: true, overlap: overlap}
		runtime := &appRuntime{hooks: map[string]*jobState{"deploy": state}}
		if err := runtime.runHook("deploy", time.Now()); err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatalf("overlap %v: run during restart = %v", overlap, err)
		}
	}
}

func TestSnapshotReportsGitConnectionWithoutABranchURL(t *testing.T) {
	cfg := hookConfig(t, [2]int{32920, 32940}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy: true\n")
	gitDir := filepath.Join(cfg.Apps, "demo", ".git")
	if err := os.Mkdir(gitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"HEAD":   "4f2a9c0d\n",
		"config": "[remote \"origin\"]\n\turl = /srv/git/app.git\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	snapshot, err := manager.Snapshot("demo")
	if err != nil || !snapshot.GitConnected || snapshot.BranchURL != "" {
		t.Fatalf("git snapshot = %+v, %v", snapshot, err)
	}
}

func TestHookWithRestartStartsTheApp(t *testing.T) {
	cfg := hookConfig(t, [2]int{32860, 32880}, "procfile:\n  web: /bin/sleep 30\nautostart: false\nhooks:\n  deploy:\n    command: /usr/bin/true\n    restart: true\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := manager.Snapshot("demo")
		started := false
		for _, process := range snapshot.Processes {
			started = started || process.Name == "web" && process.PID != 0
		}
		// The hook reads restarting until the restart has returned, so a started web process
		// and a hook that is done must be seen together eventually.
		if started && !snapshot.Hooks[0].Restarting {
			if snapshot.Hooks[0].LastError != "" {
				t.Fatalf("restart hook error: %s", snapshot.Hooks[0].LastError)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("web process was not started after the restart hook")
}

func TestHookKeepsTheOutputOfItsLastRun(t *testing.T) {
	cfg := hookConfig(t, [2]int{32880, 32900}, "procfile:\n  web: /usr/bin/true\nautostart: false\nhooks:\n  deploy:\n    command: /bin/echo not possible to fast-forward\n")
	manager, _, err := New(cfg, ports.New(cfg.Ports), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RunHook("demo", "deploy"); err != nil {
		t.Fatal(err)
	}
	waitForHookEnd(t, manager, "demo", "deploy")
	infos, err := manager.Hooks("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Output != "not possible to fast-forward\n" {
		t.Fatalf("hook output = %+v", infos)
	}
}

func TestOutputTailKeepsTheEnd(t *testing.T) {
	var tail outputTail
	_, _ = tail.Write([]byte(strings.Repeat("a", hookOutputTail)))
	_, _ = tail.Write([]byte("end"))
	if got := tail.String(); len(got) != hookOutputTail || !strings.HasSuffix(got, "aend") {
		t.Fatalf("tail = %d bytes ending %q", len(got), got[len(got)-4:])
	}
	tail.Reset()
	if tail.String() != "" {
		t.Fatal("reset kept output")
	}
}
