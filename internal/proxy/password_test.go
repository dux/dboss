package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

// passwordSnapshot has two web processes: web inherits nothing, admin asks for a password.
func passwordSnapshot(t *testing.T, password string) supervisor.Snapshot {
	t.Helper()
	dir := t.TempDir()
	data := "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\n  admin:\n    command: ./server\n    hosts: [admin.demo.test]\n    password: " + password + "\n"
	app, err := config.ParseApp([]byte(data), filepath.Join(dir, config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	// Maintenance answers right after the gate, so a request that passes it never needs a manager.
	return supervisor.Snapshot{Name: "demo", State: supervisor.Running, Maintenance: true, Dir: dir, Pages: filepath.Join(dir, app.Pages), Hosts: app.Hosts, WebProcesses: supervisor.WebProcessSnapshots(app.WebProcesses), Web: app.Web}
}

func pageRequest(method, target string, form url.Values) *http.Request {
	var request *http.Request
	if form != nil {
		request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		request = httptest.NewRequest(method, target, nil)
	}
	request.Header.Set("Accept", "text/html")
	return request
}

func TestPasswordGate(t *testing.T) {
	handler := featureHandler()
	handler.passwords.Spacing = 0
	snapshot := passwordSnapshot(t, "secret")

	if response := serveFeature(t, handler, snapshot, pageRequest(http.MethodGet, "http://demo.test/", nil)); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("web process without a password = %d", response.Code)
	}
	response := serveFeature(t, handler, snapshot, pageRequest(http.MethodGet, "http://admin.demo.test/report?x=1", nil))
	body := response.Body.String()
	if response.Code != http.StatusUnauthorized || !strings.Contains(body, "demo is protected") || !strings.Contains(body, `name="to" value="/report?x=1"`) {
		t.Fatalf("password page = %d %s", response.Code, body)
	}
	api := httptest.NewRequest(http.MethodGet, "http://admin.demo.test/api", nil)
	if response := serveFeature(t, handler, snapshot, api); response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "<form") {
		t.Fatalf("api request = %d %s", response.Code, response.Body.String())
	}

	wrong := serveFeature(t, handler, snapshot, pageRequest(http.MethodPost, "http://admin.demo.test"+passwordPath, url.Values{"password": {"nope"}, "to": {"/report"}}))
	if wrong.Code != http.StatusUnauthorized || !strings.Contains(wrong.Body.String(), "Wrong password.") || len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("wrong password = %d %s", wrong.Code, wrong.Body.String())
	}
	right := serveFeature(t, handler, snapshot, pageRequest(http.MethodPost, "http://admin.demo.test"+passwordPath, url.Values{"password": {"secret"}, "to": {"//evil.test/"}}))
	cookies := right.Result().Cookies()
	if right.Code != http.StatusSeeOther || right.Header().Get("Location") != "/" || len(cookies) != 1 || cookies[0].Name != passwordCookie {
		t.Fatalf("right password = %d %q %v", right.Code, right.Header().Get("Location"), cookies)
	}

	signedIn := pageRequest(http.MethodGet, "http://admin.demo.test/report", nil)
	signedIn.AddCookie(cookies[0])
	if response := serveFeature(t, handler, snapshot, signedIn); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("signed-in request = %d", response.Code)
	}
	// A new password signs everyone out.
	changed := passwordSnapshot(t, "other")
	if response := serveFeature(t, handler, changed, signedIn); response.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after a password change = %d", response.Code)
	}
}

func TestBasicAuthPerWebProcess(t *testing.T) {
	dir := t.TempDir()
	data := "procfile:\n  web:\n    command: ./server\n    hosts: [demo.test]\n    basic_auth: {}\n  admin:\n    command: ./server\n    hosts: [admin.demo.test]\nbasic_auth:\n  alice: secret\n"
	app, err := config.ParseApp([]byte(data), filepath.Join(dir, config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := supervisor.Snapshot{Name: "demo", State: supervisor.Running, Maintenance: true, Dir: dir, Hosts: app.Hosts, WebProcesses: supervisor.WebProcessSnapshots(app.WebProcesses), Web: app.Web}
	if response := serveFeature(t, featureHandler(), snapshot, httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("web without basic_auth = %d", response.Code)
	}
	admin := httptest.NewRequest(http.MethodGet, "http://admin.demo.test/", nil)
	if response := serveFeature(t, featureHandler(), snapshot, admin); response.Code != http.StatusUnauthorized {
		t.Fatalf("admin without credentials = %d", response.Code)
	}
	admin.SetBasicAuth("alice", "secret")
	if response := serveFeature(t, featureHandler(), snapshot, admin); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin with credentials = %d", response.Code)
	}
}

func TestPasswordGateAnswers429PastTheQueue(t *testing.T) {
	handler := featureHandler()
	snapshot := passwordSnapshot(t, "secret")
	for range 12 {
		handler.passwords.Reserve("192.0.2.1")
	}
	response := serveFeature(t, handler, snapshot, pageRequest(http.MethodPost, "http://admin.demo.test"+passwordPath, url.Values{"password": {"secret"}}))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" || !strings.Contains(response.Body.String(), "Too many attempts") || len(response.Result().Cookies()) != 0 {
		t.Fatalf("queued past the cap = %d %q %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}
