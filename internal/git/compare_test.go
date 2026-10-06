package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func compareGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=test"}, args...)
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func compareRepo(t *testing.T) (source, checkout string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	source, checkout = filepath.Join(root, "source"), filepath.Join(root, "checkout")
	compareGit(t, root, "init", "-q", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "app.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compareGit(t, source, "add", "app.txt")
	compareGit(t, source, "commit", "-q", "-m", "Initial app")
	if err := Clone(checkout, source, "", ""); err != nil {
		t.Fatal(err)
	}
	return source, checkout
}

func TestCompareCountsFreshRemoteAndLocalCommitsWithoutDeploying(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ahead, behind int
		dirty         bool
	}{
		{"equal", 0, 0, false},
		{"remote ahead", 0, 2, false},
		{"local ahead", 2, 0, false},
		{"diverged and dirty", 1, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, checkout := compareRepo(t)
			compareGit(t, checkout, "checkout", "-q", "-b", "vibe", "--track", "origin/main")
			for range tc.behind {
				compareGit(t, source, "commit", "-q", "--allow-empty", "-m", "Remote change")
			}
			for range tc.ahead {
				compareGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "Local change")
			}
			contents := "original\n"
			if tc.dirty {
				contents = "uncommitted work\n"
				if err := os.WriteFile(filepath.Join(checkout, "app.txt"), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			local := compareGit(t, checkout, "rev-parse", "HEAD")
			remote := compareGit(t, source, "rev-parse", "HEAD")
			fetchHead := filepath.Join(checkout, ".git", "FETCH_HEAD")
			if err := os.WriteFile(fetchHead, []byte("previous fetch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := Compare(context.Background(), checkout, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if result.Branch != "vibe" || result.Upstream != "origin/main" || result.Local.Hash != local || result.Remote.Hash != remote || result.Ahead != tc.ahead || result.Behind != tc.behind || result.Dirty != tc.dirty {
				t.Fatalf("comparison = %+v", result)
			}
			if result.Local.Subject == "" || result.Remote.Subject == "" {
				t.Fatalf("missing commit subjects: %+v", result)
			}
			if got := compareGit(t, checkout, "rev-parse", "HEAD"); got != local {
				t.Fatalf("preview moved local HEAD to %s", got)
			}
			if data, err := os.ReadFile(filepath.Join(checkout, "app.txt")); err != nil || string(data) != contents {
				t.Fatalf("preview changed working tree: %q, %v", data, err)
			}
			if data, err := os.ReadFile(fetchHead); err != nil || string(data) != "previous fetch\n" {
				t.Fatalf("preview changed FETCH_HEAD: %q, %v", data, err)
			}
		})
	}
}

func TestCompareUsesTheConfiguredUpstreamRemote(t *testing.T) {
	source, checkout := compareRepo(t)
	compareGit(t, checkout, "remote", "add", "upstream", source)
	compareGit(t, checkout, "fetch", "-q", "upstream")
	compareGit(t, checkout, "branch", "--set-upstream-to=upstream/main")
	compareGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unreachable"))
	compareGit(t, source, "commit", "-q", "--allow-empty", "-m", "New upstream commit")
	result, err := Compare(context.Background(), checkout, "", "")
	if err != nil || result.Upstream != "upstream/main" || result.Behind != 1 {
		t.Fatalf("comparison = %+v, %v", result, err)
	}
}

func TestCompareDeepensShallowHistoryForExactCounts(t *testing.T) {
	source, _ := compareRepo(t)
	for range 3 {
		compareGit(t, source, "commit", "-q", "--allow-empty", "-m", "Older history")
	}
	checkout := filepath.Join(t.TempDir(), "shallow")
	compareGit(t, source, "clone", "-q", "--depth", "1", "file://"+source, checkout)
	compareGit(t, source, "commit", "-q", "--allow-empty", "-m", "Newest commit")
	result, err := Compare(context.Background(), checkout, "", "")
	if err != nil || result.Behind != 1 || result.Ahead != 0 {
		t.Fatalf("comparison = %+v, %v", result, err)
	}
}

func TestCompareErrorsAreActionableAndCancellationIsRespected(t *testing.T) {
	_, checkout := compareRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Compare(ctx, checkout, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled comparison = %v", err)
	}
	if _, err := Compare(context.Background(), checkout, "", "release"); err == nil || !strings.Contains(err.Error(), `the app requires "release"`) {
		t.Fatalf("wrong branch = %v", err)
	}
	compareGit(t, checkout, "branch", "--unset-upstream")
	if _, err := Compare(context.Background(), checkout, "", ""); err == nil || !strings.Contains(err.Error(), "remote upstream") {
		t.Fatalf("missing upstream = %v", err)
	}
	compareGit(t, checkout, "checkout", "-q", "--detach")
	if _, err := Compare(context.Background(), checkout, "", ""); err == nil || !strings.Contains(err.Error(), "on a branch") {
		t.Fatalf("detached checkout = %v", err)
	}
}
