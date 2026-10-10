package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/fault"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func findChange(changes []Change, path string) (Change, bool) {
	for _, change := range changes {
		if change.Path == path {
			return change, true
		}
	}
	return Change{}, false
}

func TestStatusListsChangesAndUnpushedCommits(t *testing.T) {
	_, checkout := compareRepo(t)
	compareGit(t, checkout, "config", "user.email", "t@t")
	compareGit(t, checkout, "config", "user.name", "test")
	writeFile(t, filepath.Join(checkout, "lib.txt"), "one\ntwo\n")
	compareGit(t, checkout, "add", "lib.txt")
	compareGit(t, checkout, "commit", "-q", "-m", "Add lib")
	writeFile(t, filepath.Join(checkout, "app.txt"), "changed\nmore\n")
	writeFile(t, filepath.Join(checkout, "new.txt"), "a\nb\nc")
	writeFile(t, filepath.Join(checkout, ".gitignore"), "secret.env\n")
	writeFile(t, filepath.Join(checkout, "secret.env"), "KEY=1\n")
	compareGit(t, checkout, "mv", "lib.txt", "moved.txt")

	status, err := Status(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" || status.Upstream != "origin/main" || status.Ahead != 1 {
		t.Fatalf("branch = %q upstream = %q ahead = %d", status.Branch, status.Upstream, status.Ahead)
	}
	if app, ok := findChange(status.Changes, "app.txt"); !ok || app.Status != "M" || app.Additions != 2 || app.Deletions != 1 {
		t.Errorf("app.txt = %+v", app)
	}
	if added, ok := findChange(status.Changes, "new.txt"); !ok || added.Status != "?" || added.Additions != 3 {
		t.Errorf("new.txt = %+v", added)
	}
	if moved, ok := findChange(status.Changes, "moved.txt"); !ok || moved.Status != "R" || moved.OldPath != "lib.txt" {
		t.Errorf("moved.txt = %+v", moved)
	}
	if _, ok := findChange(status.Changes, "secret.env"); ok {
		t.Error("an ignored file is listed as a change")
	}
	if len(status.Unpushed) != 1 || status.Unpushed[0].Subject != "Add lib" || len(status.Unpushed[0].Files) != 1 || status.Unpushed[0].Files[0].Status != "A" {
		t.Fatalf("unpushed = %+v", status.Unpushed)
	}

	diff, err := Diff(context.Background(), checkout, Change{Path: "app.txt", Status: "M"})
	if err != nil || !strings.Contains(diff, "-original") || !strings.Contains(diff, "+changed") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	untracked, err := Diff(context.Background(), checkout, Change{Path: "new.txt", Status: "?"})
	if err != nil || !strings.Contains(untracked, "+a") {
		t.Fatalf("untracked diff = %q, %v", untracked, err)
	}
	committed, err := CommitDiff(context.Background(), checkout, status.Unpushed[0].Hash, Change{Path: "lib.txt"})
	if err != nil || !strings.Contains(committed, "+one") {
		t.Fatalf("commit diff = %q, %v", committed, err)
	}
}

func TestCommitAllAndResetLeaveIgnoredFiles(t *testing.T) {
	_, checkout := compareRepo(t)
	writeFile(t, filepath.Join(checkout, ".gitignore"), "secret.env\n")
	writeFile(t, filepath.Join(checkout, "secret.env"), "KEY=1\n")
	writeFile(t, filepath.Join(checkout, "app.txt"), "changed\n")
	writeFile(t, filepath.Join(checkout, "new.txt"), "new\n")

	// The checkout has no identity of its own, so the commit takes the author handed in.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	hash, err := CommitAll(context.Background(), checkout, "Vibe change", "dboss vibe <vibe@demo.test>")
	if err != nil || hash == "" {
		t.Fatalf("commit = %q, %v", hash, err)
	}
	if author := compareGit(t, checkout, "log", "-1", "--format=%an <%ae>"); author != "dboss vibe <vibe@demo.test>" {
		t.Errorf("author = %q", author)
	}
	if tracked := compareGit(t, checkout, "ls-files"); strings.Contains(tracked, "secret.env") || !strings.Contains(tracked, "new.txt") {
		t.Errorf("tracked files = %q", tracked)
	}
	if _, err := CommitAll(context.Background(), checkout, "again", ""); !fault.IsInvalid(err) {
		t.Errorf("an empty commit = %v, want an invalid error", err)
	}

	writeFile(t, filepath.Join(checkout, "app.txt"), "broken\n")
	writeFile(t, filepath.Join(checkout, "junk.txt"), "junk\n")
	stash, err := Reset(context.Background(), checkout, "dboss vibe: reset", "dboss vibe <vibe@demo.test>")
	if err != nil || stash != "stash@{0}" {
		t.Fatalf("reset = %q, %v", stash, err)
	}
	if data, _ := os.ReadFile(filepath.Join(checkout, "app.txt")); string(data) != "changed\n" {
		t.Errorf("app.txt after reset = %q", data)
	}
	if _, err := os.Stat(filepath.Join(checkout, "junk.txt")); !os.IsNotExist(err) {
		t.Error("an untracked file survived the reset")
	}
	if _, err := os.Stat(filepath.Join(checkout, "secret.env")); err != nil {
		t.Error("the reset removed an ignored file")
	}
	if _, err := Reset(context.Background(), checkout, "again", ""); !fault.IsInvalid(err) {
		t.Errorf("a reset with nothing to reset = %v, want an invalid error", err)
	}
	compareGit(t, checkout, "stash", "pop")
	if data, _ := os.ReadFile(filepath.Join(checkout, "junk.txt")); string(data) != "junk\n" {
		t.Errorf("stash pop did not bring the work back: %q", data)
	}
}

func TestPushSendsToTheUpstreamAndRefusesOtherBranches(t *testing.T) {
	source, checkout := compareRepo(t)
	compareGit(t, source, "checkout", "-q", "-b", "other")
	writeFile(t, filepath.Join(checkout, "app.txt"), "pushed\n")
	compareGit(t, checkout, "commit", "-q", "-am", "Push me")
	if _, err := Push(context.Background(), checkout, "", "release"); !fault.IsInvalid(err) {
		t.Fatalf("push on another branch = %v, want an invalid error", err)
	}
	if _, err := Push(context.Background(), checkout, "", "main"); err != nil {
		t.Fatal(err)
	}
	if subject := compareGit(t, source, "log", "-1", "--format=%s", "main"); subject != "Push me" {
		t.Fatalf("source main = %q", subject)
	}
	compareGit(t, checkout, "checkout", "-q", "-b", "local")
	if _, err := Push(context.Background(), checkout, "", ""); !fault.IsInvalid(err) {
		t.Fatalf("push without an upstream = %v, want an invalid error", err)
	}
}

// A folder inside a larger checkout is refused, so a reset never stashes the other projects' work.
func TestWorktreeActionsRefuseAFolderInsideALargerRepository(t *testing.T) {
	_, checkout := compareRepo(t)
	sub := filepath.Join(checkout, "apps", "shop")
	writeFile(t, filepath.Join(sub, "index.html"), "hi\n")
	writeFile(t, filepath.Join(checkout, "app.txt"), "outer work\n")
	ctx := context.Background()
	if _, err := Status(ctx, sub); !fault.IsInvalid(err) {
		t.Errorf("status = %v, want an invalid error", err)
	}
	if _, err := CommitAll(ctx, sub, "msg", ""); !fault.IsInvalid(err) {
		t.Errorf("commit = %v, want an invalid error", err)
	}
	if _, err := Push(ctx, sub, "", ""); !fault.IsInvalid(err) {
		t.Errorf("push = %v, want an invalid error", err)
	}
	if _, err := Reset(ctx, sub, "msg", ""); !fault.IsInvalid(err) {
		t.Errorf("reset = %v, want an invalid error", err)
	}
	if data, _ := os.ReadFile(filepath.Join(checkout, "app.txt")); string(data) != "outer work\n" {
		t.Errorf("outer work = %q", data)
	}
}
