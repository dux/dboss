package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"dboss/internal/ops"
	"dboss/internal/supervisor"
)

func apiPost(t *testing.T, handler http.Handler, path, token, body string) (int, apiResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081"+path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var decoded apiResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s: %v: %s", path, err, response.Body.String())
	}
	return response.Code, decoded
}

func TestAPINeedsTheDbossToken(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}}
	handler := newTestHandler(t, manager, nil)

	if code, response := apiPost(t, handler, "/api/restart", "", `{"app":"web"}`); code != http.StatusBadRequest || response.Error.Code != apiDisabled {
		t.Fatalf("without tokens.dboss = %d %+v", code, response.Error)
	}
	manager.token = "s3cret"
	if code, response := apiPost(t, handler, "/api/restart", "wrong", `{"app":"web"}`); code != http.StatusBadRequest || response.Error.Code != apiUnauthorized {
		t.Fatalf("wrong token = %d %+v", code, response.Error)
	}
	if len(manager.actions) != 0 {
		t.Fatalf("a refused call ran %v", manager.actions)
	}
	code, response := apiPost(t, handler, "/api/restart", "s3cret", `{"app":"web"}`)
	if code != http.StatusOK || !response.OK || !slices.Contains(manager.actions, "restart web") {
		t.Fatalf("restart = %d %+v, actions %v", code, response, manager.actions)
	}
}

func TestAPIChecksParamsAgainstTheSpec(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}, token: "s3cret"}
	handler := newTestHandler(t, manager, nil)
	cases := []struct{ path, body, code, message string }{
		{"/api/nope", `{}`, apiUnknownAction, "unknown action"},
		{"/api/restart", `{"app":"web","color":"red"}`, apiInvalidRequest, `no param "color"`},
		{"/api/restart", `{}`, apiInvalidRequest, "restart needs app"},
		{"/api/restart", `[1]`, apiInvalidRequest, "one JSON object"},
		{"/api/restart", `{"app":7}`, apiInvalidRequest, "cannot unmarshal"},
		{"/api/restart", `{"app":"web","method":"destroy"}`, apiInvalidRequest, `no param "method"`},
		{"/api/exec", `{"app":"web","argv":["ls"],"timeout":"soon"}`, apiInvalidRequest, "timeout"},
		{"/api/status", `{"app":"ghost"}`, apiFailed, "unknown app"},
	}
	for _, c := range cases {
		code, response := apiPost(t, handler, c.path, "s3cret", c.body)
		if code != http.StatusBadRequest || response.OK || response.Error == nil || response.Error.Code != c.code || !strings.Contains(response.Error.Message, c.message) {
			t.Errorf("%s %s = %d %+v, want %s containing %q", c.path, c.body, code, response.Error, c.code, c.message)
		}
	}
	if len(manager.actions) != 0 {
		t.Fatalf("a refused call ran %v", manager.actions)
	}
}

func TestAPIReadsEmptyBodiesAndDurations(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}, token: "s3cret"}
	handler := newTestHandler(t, manager, nil)

	code, response := apiPost(t, handler, "/api/ls", "s3cret", "")
	if code != http.StatusOK || !strings.Contains(string(mustJSON(t, response.Data)), `"name":"web"`) {
		t.Fatalf("ls = %d %+v", code, response)
	}
	if code, response := apiPost(t, handler, "/api/exec", "s3cret", `{"app":"web","argv":["ls","-la"],"timeout":"30s"}`); code != http.StatusOK {
		t.Fatalf("exec = %d %+v", code, response.Error)
	}
	if !slices.Contains(manager.actions, "exec web ls -la (30s)") {
		t.Fatalf("actions = %v", manager.actions)
	}
}

func TestAPIGuideAndOpenAPIAreOpen(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	for _, path := range []string{"/api", "/api/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081"+path, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "#### POST /api/restart (audited)") || !strings.Contains(body, "The API is off") || !strings.Contains(body, "| [/api/openapi.json](/api/openapi.json) ") || !strings.Contains(body, "| [/healthz](/healthz) ") {
			t.Fatalf("GET %s = %d: %s", path, response.Code, body)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081"+openAPIPath, nil))
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Servers []struct{ URL string }    `json:"servers"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil || response.Code != http.StatusOK {
		t.Fatalf("openapi = %d %v", response.Code, err)
	}
	if doc.OpenAPI != "3.1.0" || len(doc.Servers) != 1 || doc.Servers[0].URL != "http://dboss.lvh.me:8081" {
		t.Fatalf("openapi header = %+v", doc)
	}
	for _, spec := range ops.Specs() {
		if doc.Paths["/api/"+spec.Name]["post"] == nil {
			t.Errorf("openapi has no POST /api/%s", spec.Name)
		}
	}
}

const chromeAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

func TestAPIGuideIsAPageForBrowsers(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	get := func(path, agent, accept string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081"+path, nil)
		request.Header.Set("User-Agent", agent)
		request.Header.Set("Accept", accept)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	page := get("/api", chromeAgent, "text/html,application/xhtml+xml,*/*;q=0.8")
	body := page.Body.String()
	if !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, `<ui-markdown src="api-guide">`) {
		t.Fatalf("browser GET /api = %s: %s", page.Header().Get("Content-Type"), body)
	}
	start := strings.Index(body, `id="api-guide">`) + len(`id="api-guide">`)
	var guide string
	if err := json.Unmarshal([]byte(body[start:start+strings.Index(body[start:], "</script>")]), &guide); err != nil || !strings.HasPrefix(guide, "# dboss API") {
		t.Fatalf("embedded guide = %q, %v", guide, err)
	}
	for _, c := range []struct{ path, agent, accept string }{
		{"/api?format=md", chromeAgent, "text/html"},
		{"/api", "curl/8.7.1", "*/*"},
		{"/api", "Claude-User", "text/html"},
	} {
		if response := get(c.path, c.agent, c.accept); !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s as %s = %s, want the markdown", c.path, c.agent, response.Header().Get("Content-Type"))
		}
	}
}

func TestAPIGuidePageAssetsAreOpen(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	for _, asset := range apiPageAssets {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/assets/"+asset, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Errorf("GET /assets/%s without a session = %d", asset, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/assets/fez/db-shell.fez", nil))
	if response.Code == http.StatusOK {
		t.Fatal("the console's own components must stay behind sign-in")
	}
}

// A raw <placeholder> in markdown renders as an unknown HTML tag and vanishes, so every one
// belongs in a code span or a fenced block.
func TestAPIGuideHasNoRawTags(t *testing.T) {
	codeSpan := regexp.MustCompile("`[^`]*`")
	fenced := false
	for number, line := range strings.Split(apiGuide("http://dboss.lvh.me", true), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if !fenced && strings.Contains(codeSpan.ReplaceAllString(line, ""), "<") {
			t.Errorf("line %d has a raw <: %s", number+1, line)
		}
	}
}

func TestAPIWrongVerbPointsAtTheGuide(t *testing.T) {
	handler := newTestHandler(t, &fakeManager{}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/api/restart", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), apiUnknownAction) {
		t.Fatalf("GET /api/restart = %d: %s", response.Code, response.Body.String())
	}
}

func TestOperationID(t *testing.T) {
	for name, want := range map[string]string{"ls": "ls", "pg-backup": "pgBackup", "host-hook-run": "hostHookRun"} {
		if got := operationID(name); got != want {
			t.Errorf("operationID(%q) = %q, want %q", name, got, want)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
