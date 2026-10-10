package ops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/supervisor"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=test"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Commit and reset act on the app's own folder and leave one audit row each, the commit subject
// as its detail.
func TestGitActionsCommitResetAndAudit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &auditStore{}
	service := New(&fakeRuntime{snapshots: []supervisor.Snapshot{{Name: "web", Dir: dir, Hosts: []string{".demo.test", "demo.test"}}}}, store, nil, nil, nil, nil)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	result, err := service.Do(Request{Method: ActionGitCommit, App: "web", Message: "First\n\nbody", Actor: "vibe:web/web"})
	if err != nil || result.(GitCommitResult).Hash == "" {
		t.Fatalf("commit = %+v, %v", result, err)
	}
	if author := runGit(t, dir, "log", "-1", "--format=%ae"); author != "dboss@demo.test" {
		t.Errorf("author = %q", author)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, err := service.Do(Request{Method: ActionGitReset, App: "web", Actor: "vibe:web/web"})
	if err != nil || reset.(GitResetResult).Stash != "stash@{0}" {
		t.Fatalf("reset = %+v, %v", reset, err)
	}
	if len(store.rows) != 2 || store.rows[0].Action != ActionGitCommit || store.rows[0].Detail != "First" || store.rows[1].Action != ActionGitReset || store.rows[1].Result != "ok" {
		t.Fatalf("audit = %+v", store.rows)
	}
	if _, err := service.Do(Request{Method: ActionGitPush, App: "web"}); err == nil {
		t.Fatal("a push without an upstream should fail")
	}
}
