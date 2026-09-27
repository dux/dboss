//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		expectStatus(t, call{addr: host.console, path: path}.do(t), http.StatusOK)
		// The same routes answer on the management host through the proxy.
		expectStatus(t, get(t, "dboss.lvh.me", path), http.StatusOK)
	}
}

func TestMetricsNeedTheToken(t *testing.T) {
	if r := (call{addr: host.console, path: "/metrics"}).do(t); r.Status == http.StatusOK {
		t.Fatal("/metrics answered without a token")
	}
	r := call{addr: host.console, path: "/metrics", header: map[string]string{"Authorization": "Bearer " + token}}.do(t)
	expectStatus(t, r, http.StatusOK)
	expectBody(t, r, "# TYPE", `app="bun"`, `app="sinatra"`)
}

func TestAPIGuideAndOpenAPI(t *testing.T) {
	guide := call{addr: host.console, path: "/api"}.do(t)
	expectStatus(t, guide, http.StatusOK)
	expectBody(t, guide, "/api/status", "Bearer")
	spec := call{addr: host.console, path: "/api/openapi.json"}.do(t)
	expectStatus(t, spec, http.StatusOK)
	var doc struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal([]byte(spec.Body), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") || doc.Paths["/api/restart"] == nil {
		t.Fatalf("openapi %q with %d paths", doc.OpenAPI, len(doc.Paths))
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	cases := []struct {
		name, bearer, action string
		params               map[string]any
		code                 string
	}{
		{"wrong token", "nope", "ls", nil, "unauthorized"},
		{"unknown action", token, "reboot", nil, "unknown_action"},
		{"missing param", token, "status", nil, "invalid_request"},
		{"unknown param", token, "ls", map[string]any{"colour": "red"}, "invalid_request"},
		{"unknown app", token, "status", map[string]any{"app": "ghost"}, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, envelope := apiRaw(t, c.bearer, c.action, c.params)
			if status != http.StatusBadRequest || envelope.OK || envelope.Error == nil || envelope.Error.Code != c.code {
				t.Fatalf("answered %d %+v, want 400 %s", status, envelope.Error, c.code)
			}
		})
	}
}

func TestAPIListsEveryApp(t *testing.T) {
	var apps []snapshot
	api(t, "ls", nil, &apps)
	names := map[string]bool{}
	for _, app := range apps {
		names[app.Name] = true
	}
	for _, name := range []string{"bun", "sinatra", "button"} {
		if !names[name] {
			t.Fatalf("ls has no %s: %v", name, names)
		}
	}
}

func TestConsoleSession(t *testing.T) {
	// No session: the private JSON refuses, the page does not hand out an AuthCog redirect on loopback.
	expectStatus(t, call{addr: host.console, path: "/ui/apps"}.do(t), http.StatusUnauthorized)
	client := consoleClient(t)
	r := call{addr: host.console, path: "/ui/apps", client: client}.do(t)
	expectStatus(t, r, http.StatusOK)
	expectBody(t, r, `"bun"`, `"sinatra"`)
	page := call{addr: host.console, path: "/", client: client}.do(t)
	expectStatus(t, page, http.StatusOK)
	expectBody(t, page, "db-shell")
	// A login link works once.
	output := cli(t, "login")
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "http://"+host.console+"/login?token=") {
			path := strings.TrimPrefix(field, "http://"+host.console)
			if first := (call{addr: host.console, path: path}).do(t); first.Status >= 400 {
				t.Fatalf("first use answered %d", first.Status)
			}
			if second := (call{addr: host.console, path: path}).do(t); second.Status < 400 {
				t.Fatalf("second use answered %d", second.Status)
			}
		}
	}
}

func TestAuditRecordsAPIActions(t *testing.T) {
	ensureRunning(t, "bun")
	api(t, "restart", map[string]any{"app": "bun", "process": "admin"}, nil)
	var rows []struct {
		Actor  string `json:"actor"`
		Action string `json:"action"`
		App    string `json:"app"`
	}
	api(t, "audit", map[string]any{"by_actor": "api", "action": "restart", "app": "bun"}, &rows)
	if len(rows) == 0 || rows[0].Actor != "api" || rows[0].App != "bun" {
		t.Fatalf("audit rows %+v", rows)
	}
	// Reads are not audited.
	api(t, "audit", map[string]any{"by_actor": "api", "action": "status"}, &rows)
	if len(rows) != 0 {
		t.Fatalf("status wrote %d audit rows", len(rows))
	}
}

func TestCLIOverTheSocket(t *testing.T) {
	ensureRunning(t, "bun")
	if out := cli(t, "ls"); !strings.Contains(out, "bun") || !strings.Contains(out, "sinatra") {
		t.Fatalf("dboss ls:\n%s", out)
	}
	var s snapshot
	if err := json.Unmarshal([]byte(cli(t, "status", "bun", "--json")), &s); err != nil || s.Name != "bun" {
		t.Fatalf("dboss status --json: %v %+v", err, s)
	}
	if out := cli(t, "ports"); !strings.Contains(out, "bun") {
		t.Fatalf("dboss ports:\n%s", out)
	}
	if out := cli(t, "check"); !strings.Contains(out, "ok") {
		t.Fatalf("dboss check:\n%s", out)
	}
	if out := cli(t, "cron", "bun"); !strings.Contains(out, "heartbeat") {
		t.Fatalf("dboss cron bun:\n%s", out)
	}
	if out := cli(t, "exec", "bun", "sh", "-c", "echo $APP_NAME"); strings.TrimSpace(out) != "bun" {
		t.Fatalf("dboss exec:\n%s", out)
	}
	if out, err := cliTry("destroy", "bun"); err == nil {
		t.Fatalf("dboss destroy bun succeeded:\n%s", out)
	}
}
