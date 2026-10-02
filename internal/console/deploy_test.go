package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"dboss/internal/git"
	"dboss/internal/supervisor"
)

func TestDeployPreviewRequiresSessionAndApp(t *testing.T) {
	manager := &fakeManager{}
	handler := newTestHandler(t, manager, nil)
	cookie, _ := sessionCookie(t, handler)
	for _, tc := range []struct {
		query  string
		signed bool
		status int
	}{
		{"?app=shop", false, http.StatusUnauthorized},
		{"", true, http.StatusBadRequest},
		{"?app=missing", true, http.StatusConflict},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/ui/apps/deploy-preview"+tc.query, nil)
		if tc.signed {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s signed %v: status %d, body %s", tc.query, tc.signed, response.Code, response.Body.String())
		}
	}
	if len(manager.actions) != 0 {
		t.Fatalf("preview ran actions: %v", manager.actions)
	}
}

func TestDeployPreviewShowsFreshHeadsAndCountsWithoutRunningTheHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	source, checkout := filepath.Join(root, "source"), filepath.Join(root, "checkout")
	run := func(dir string, args ...string) {
		t.Helper()
		args = append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=t@t"}, args...)
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-q", "-b", "vibe", source)
	run(source, "commit", "-q", "--allow-empty", "-m", "Local commit")
	if err := git.Clone(checkout, source, "", ""); err != nil {
		t.Fatal(err)
	}
	run(source, "commit", "-q", "--allow-empty", "-m", "Remote change")
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "shop", Dir: checkout, GitConnected: true}}}
	handler := newTestHandler(t, manager, nil)
	cookie, _ := sessionCookie(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/ui/apps/deploy-preview?app=shop&dir=/wrong", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var preview git.Comparison
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Branch != "vibe" || preview.Upstream != "origin/vibe" || preview.Behind != 1 || preview.Ahead != 0 || preview.Local.Subject != "Local commit" || preview.Remote.Subject != "Remote change" || preview.Local.Hash == preview.Remote.Hash {
		t.Fatalf("preview = %+v", preview)
	}
	if len(manager.actions) != 0 {
		t.Fatalf("preview ran actions: %v", manager.actions)
	}
	run(checkout, "remote", "set-url", "origin", filepath.Join(root, "missing-remote"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || len(manager.actions) != 0 {
		t.Fatalf("unreadable remote: status %d, actions %v, body %s", response.Code, manager.actions, response.Body.String())
	}
}
