package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/ctl"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestSyncManifestShipsTrackedFilesOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"dboss.yaml": "procfile:\n  web: ./server\n",
		"app.rb":     "ok",
		"gone.rb":    "tracked, then deleted from the tree",
		".gitignore": "log/\n.env.local\n",
	})
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	if err := os.Remove(filepath.Join(dir, "gone.rb")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{
		".env.local":     "SECRET=1",
		"log/app.log":    "ignored",
		".env":           "untracked",
		"scratch/a.txt":  "untracked",
		"scratch/b.txt":  "untracked",
		syncManifestName: "never shipped back",
	})

	files, untracked, err := syncManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".gitignore", "app.rb", "dboss.yaml"}; !slices.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	// .env, scratch/ and the manifest; ignored files are not counted.
	if untracked != 3 {
		t.Fatalf("untracked = %d, want 3", untracked)
	}
}

func TestSyncManifestNeedsARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if _, _, err := syncManifest(dir); err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployApplyRemovesOnlyWhatItShipped(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"dboss.yaml":     "procfile:\n  web: ./server\n",
		"app.rb":         "kept",
		"lib/old/x.rb":   "dropped by the app",
		"log/app.log":    "box only",
		".env.local":     "box only, once shipped",
		syncManifestName: "dboss.yaml\napp.rb\nlib/old/x.rb\n.env.local\n",
	})
	next := "dboss.yaml\napp.rb\n"

	var out bytes.Buffer
	dry := CLI{In: strings.NewReader(next), Out: &out, Err: &out}
	if err := dry.deployApply([]string{root, "-n"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "would remove lib/old/x.rb" {
		t.Fatalf("dry run = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "lib/old/x.rb")); err != nil {
		t.Fatal("a dry run removed a file")
	}

	out.Reset()
	cli := CLI{In: strings.NewReader(next), Out: &out, Err: &out}
	// No daemon answers here, so the restart fails after the files are settled.
	if err := cli.deployApply([]string{root, "--socket", filepath.Join(root, "none.sock")}); err == nil {
		t.Fatal("restart without a daemon succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "lib")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied directories were kept: %v", err)
	}
	for _, kept := range []string{"app.rb", "log/app.log", ".env.local"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("%s was removed", kept)
		}
	}
	manifest, _ := os.ReadFile(filepath.Join(root, syncManifestName))
	if string(manifest) != next {
		t.Fatalf("manifest = %q", manifest)
	}
}

// fakeDaemon answers every control request with ok and reports each method on the channel.
func fakeDaemon(t *testing.T) (string, chan string) {
	t.Helper()
	// A short directory keeps the socket path under the macOS sun_path limit.
	dir, err := os.MkdirTemp("", "dboss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	methods := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request ctl.Request
			_ = json.NewDecoder(conn).Decode(&request)
			methods <- request.Method
			_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
			conn.Close()
		}
	}()
	return socket, methods
}

func TestDeployApplyRescansBeforeRestart(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"dboss.yaml": "procfile:\n  web: ./server\n"})
	socket, methods := fakeDaemon(t)

	var out bytes.Buffer
	cli := CLI{In: strings.NewReader("dboss.yaml\n"), Out: &out, Err: &out}
	if err := cli.deployApply([]string{root, "--app", "demo", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	if first, second := <-methods, <-methods; first != "rescan" || second != "restart" {
		t.Fatalf("calls = %s, %s; want rescan, restart", first, second)
	}
}

func TestDeployApplyHostRescansAndNeverRestarts(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"dboss-server.yaml":       "proxy:\n  listen: \":80\"\n",
		"dboss-server.local.yaml": "proxy:\n  listen: \":8080\"\n",
		"pages/old.html":          "dropped",
		"apps/shop/dboss.yaml":    "procfile:\n  web: ./server\n",
		syncManifestName:          "dboss-server.yaml\ndboss-server.local.yaml\npages/old.html\n",
	})
	socket, methods := fakeDaemon(t)

	var out, errOut bytes.Buffer
	cli := CLI{In: strings.NewReader("dboss-server.yaml\n"), Out: &out, Err: &errOut}
	if err := cli.deployApply([]string{root, "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	if method := <-methods; method != "rescan" {
		t.Fatalf("first call = %s, want rescan", method)
	}
	select {
	case method := <-methods:
		t.Fatalf("a host apply also sent %s", method)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(root, "pages")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file the host no longer ships was kept: %v", err)
	}
	for _, kept := range []string{"dboss-server.local.yaml", "apps/shop/dboss.yaml"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("%s was removed", kept)
		}
	}
	if !strings.Contains(errOut.String(), "replaces dboss-server.yaml entirely") || !strings.Contains(errOut.String(), "it differs at: proxy.listen") {
		t.Fatalf("no override warning: %q", errOut.String())
	}
}

func TestDeployApplyRefusesAnInvalidHostFile(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"dboss-server.yaml": "proxy:\n  listen: 80\n",
		"old.txt":           "x",
		syncManifestName:    "dboss-server.yaml\nold.txt\n",
	})
	cli := CLI{In: strings.NewReader("dboss-server.yaml\n"), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if err := cli.deployApply([]string{root, "--socket", filepath.Join(root, "none.sock")}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("an invalid host file was applied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); err != nil {
		t.Fatal("a refused apply removed a file")
	}
	// The next sync must still know what the last good one shipped.
	if manifest, _ := os.ReadFile(filepath.Join(root, syncManifestName)); string(manifest) != "dboss-server.yaml\nold.txt\n" {
		t.Fatalf("manifest = %q", manifest)
	}
}

func TestCheckHostManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dboss-server.yaml")
	writeFiles(t, dir, map[string]string{"dboss-server.yaml": "apps: ./apps\n"})
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHostManifest(cfg, path, []string{"dboss-server.yaml", "public/error_pages/error.html"}); err != nil {
		t.Fatal(err)
	}
	if err := checkHostManifest(cfg, path, []string{"README.md"}); err == nil || !strings.Contains(err.Error(), "git add") {
		t.Fatalf("a sync without the host file was accepted: %v", err)
	}
	for _, owned := range []string{"apps/shop/dboss.yaml", ".dboss/state/running.json"} {
		if err := checkHostManifest(cfg, path, []string{"dboss-server.yaml", owned}); err == nil {
			t.Fatalf("%s was accepted", owned)
		}
	}
	// A runtime dir outside the folder (dir: /var/lib/dboss) owns nothing in it.
	writeFiles(t, dir, map[string]string{"dboss-server.yaml": "dir: /var/lib/dboss\napps: /srv/apps\n"})
	if cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := checkHostManifest(cfg, path, []string{"dboss-server.yaml", "apps/notes.md"}); err != nil {
		t.Fatal(err)
	}
}

func TestDifferingKeys(t *testing.T) {
	base := []byte("proxy:\n  listen: \":80\"\n  cloudflare: true\nmanagement:\n  admins: [a@b.c]\n")
	local := []byte("proxy:\n  listen: \":80\"\n  cloudflare: false\nmanagement:\n  admins: [a@b.c, d@e.f]\npostgres: false\n")
	if got := strings.Join(differingKeys(base, local), ","); got != "management.admins,postgres,proxy.cloudflare" {
		t.Fatalf("got %s", got)
	}
	if keys := differingKeys(base, base); len(keys) != 0 {
		t.Fatalf("equal files differ at %v", keys)
	}
}

func TestDeployApplyRefusesAFolderWithoutAnApp(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"keep.txt": "x", syncManifestName: "keep.txt\n"})
	cli := CLI{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if err := cli.deployApply([]string{root}); err == nil {
		t.Fatal("apply ran in a folder without an app config")
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatal("apply removed a file outside an app")
	}
}

func TestReadManifestStaysInTheAppFolder(t *testing.T) {
	for _, entry := range []string{"../x", "/etc/passwd", "a/../../x"} {
		if _, err := readManifest(strings.NewReader(entry + "\n")); err == nil {
			t.Fatalf("%q was accepted", entry)
		}
	}
}

func TestRemoveShippedDoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFiles(t, outside, map[string]string{"f.rb": "not ours"})
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := removeShipped(root, filepath.Join("link", "f.rb")); err == nil {
		t.Fatal("removed through a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "f.rb")); err != nil {
		t.Fatal("a file outside the app was removed")
	}
}

func TestSplitTarget(t *testing.T) {
	host, path, err := splitTarget("deploy@box.example.com:/srv/dboss/apps/myapp/")
	if err != nil || host != "deploy@box.example.com" || path != "/srv/dboss/apps/myapp" {
		t.Fatalf("got %q %q %v", host, path, err)
	}
	for _, bad := range []string{"box", "box:relative/path", ":/srv/x", "-oProxyCommand=x:/srv/x", "box:/", "box:/srv/my app", "box:/srv/$(id)"} {
		if _, _, err := splitTarget(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if got := rsyncArgs("box", "/srv/a", true); !slices.Equal(got, []string{"-az", "--files-from=-", "-n", "-v", "./", "box:/srv/a/"}) {
		t.Fatalf("rsync args = %v", got)
	}
	if got := applyArgs("box", "/srv/a", "a", false); !slices.Equal(got, []string{"box", "dboss", "deploy", "apply", "/srv/a", "--app", "a"}) {
		t.Fatalf("apply args = %v", got)
	}
}

// hookServer answers the deploy ping with 202 and then serves the given status bodies in turn,
// repeating the last one.
func hookServer(t *testing.T, statuses ...string) *httptest.Server {
	t.Helper()
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/hooks/demo/deploy" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte(statuses[min(polls, len(statuses)-1)]))
		polls++
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDeployGitWaitsForThePullAndTheRestart(t *testing.T) {
	previous := deployPoll
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = previous })
	server := hookServer(t,
		`{"name":"deploy","running":true,"last_exit":0}`,
		`{"name":"deploy","running":false,"restarting":true,"last_exit":0}`,
		`{"name":"deploy","running":false,"last_exit":0,"output":"Fast-forward\n"}`,
	)
	var out, errOut bytes.Buffer
	if err := (CLI{Out: &out, Err: &errOut}).deployGit([]string{server.URL + "/", "--app", "demo", "--token", "tok"}); err != nil {
		t.Fatalf("%v: %s", err, errOut.String())
	}
	if out.String() != "Fast-forward\ndeployed demo: pulled and restarted\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestDeployGitReportsAFailedPull(t *testing.T) {
	previous := deployPoll
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = previous })
	server := hookServer(t, `{"name":"deploy","running":false,"last_exit":128,"last_error":"exit code 128","output":"fatal: Not possible to fast-forward, aborting.\n"}`)
	var out, errOut bytes.Buffer
	err := (CLI{Out: &out, Err: &errOut}).deployGit([]string{server.URL, "--app", "demo", "--token", "tok"})
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 128 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "Not possible to fast-forward") || !strings.Contains(errOut.String(), "deploy of demo failed: exit code 128") {
		t.Fatalf("out = %q, err = %q", out.String(), errOut.String())
	}
}

func TestDeployGitExplainsAMissingHookAndABadToken(t *testing.T) {
	server := hookServer(t, `{}`)
	cli := CLI{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if err := cli.deployGit([]string{server.URL, "--app", "other", "--token", "tok"}); err == nil || !strings.Contains(err.Error(), "hooks: {deploy: true}") {
		t.Fatalf("missing hook: %v", err)
	}
	if err := cli.deployGit([]string{server.URL, "--app", "demo", "--token", "nope"}); err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("bad token: %v", err)
	}
	t.Setenv("DBOSS_TOKEN", "")
	if err := cli.deployGit([]string{server.URL, "--app", "demo"}); err == nil || !strings.Contains(err.Error(), "DBOSS_TOKEN") {
		t.Fatalf("no token: %v", err)
	}
}

func TestCheckBranchRefusesAnotherBranch(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"app.txt": "x\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	if err := checkBranch(dir, "main"); err != nil {
		t.Fatal(err)
	}
	if err := checkBranch(dir, ""); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	if err := checkBranch(dir, "main"); err == nil || !strings.Contains(err.Error(), `on "feature", the app requires "main"`) {
		t.Fatalf("feature = %v", err)
	}
	gitRun(t, dir, "checkout", "-q", "--detach")
	if err := checkBranch(dir, "main"); err == nil || !strings.Contains(err.Error(), "this one has none") {
		t.Fatalf("detached = %v", err)
	}
}
