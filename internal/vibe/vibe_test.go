package vibe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/supervisor"
)

type fakeOps struct {
	mu       sync.Mutex
	snapshot supervisor.Snapshot
	requests []ops.Request
	audits   []string
}

func (f *fakeOps) Do(request ops.Request) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	switch request.Method {
	case ops.ActionStatus:
		return f.snapshot, nil
	case ops.ActionExec:
		return supervisor.ExecResult{Output: "ran " + strings.Join(request.Argv, " "), ExitCode: 0}, nil
	case ops.ActionGitCommit:
		return ops.GitCommitResult{Hash: "abc1234"}, nil
	}
	return nil, nil
}

func (f *fakeOps) Audit(actor, app, action, detail string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, actor+" "+action+" "+detail)
}

func (f *fakeOps) SearchLogs(string, logstore.LogFilter) ([]logstore.LogEntry, error) {
	return []logstore.LogEntry{{Time: time.Now(), Level: "info", Process: "web", Raw: "GET / 200"}}, nil
}

func (f *fakeOps) Exceptions(string, logstore.ExceptionFilter) ([]logstore.ExceptionSummary, error) {
	return nil, nil
}

func (f *fakeOps) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var methods []string
	for _, request := range f.requests {
		if request.Method != ops.ActionStatus {
			methods = append(methods, request.Method+":"+request.Actor)
		}
	}
	return methods
}

func testSnapshot(dir string) supervisor.Snapshot {
	return supervisor.Snapshot{
		Name:  "shop",
		Dir:   dir,
		State: supervisor.Running,
		WebProcesses: []supervisor.WebProcessSnapshot{
			{Name: "web", Hosts: []string{"shop.test"}, Vibe: config.Vibe{Enabled: true}},
			{Name: "admin", Hosts: []string{"admin.shop.test"}},
		},
		Web: config.Web{SessionTTL: config.Duration(time.Hour)},
	}
}

func newTestService(t *testing.T) (*Service, *fakeOps, string) {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeOps{snapshot: testSnapshot(dir)}
	service, err := New(fake, t.TempDir(), filepath.Join(dir, ".dboss"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, fake, dir
}

func serve(service *Service, app supervisor.Snapshot, request *http.Request) (*httptest.ResponseRecorder, bool) {
	recorder := httptest.NewRecorder()
	passed := false
	service.Filter(recorder, request, app, func() { passed = true })
	return recorder, passed
}

func TestConfineKeepsPathsInsideTheApp(t *testing.T) {
	service, _, dir := newTestService(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../x", "/etc/passwd", "escape/file", ".git/config", ".dboss/state/x", "a/../../x"} {
		if _, err := service.confine(dir, bad); err == nil {
			t.Errorf("%q was allowed", bad)
		}
	}
	for path, want := range map[string]string{"app.rb": "app.rb", "lib/../app.rb": "app.rb", filepath.Join(dir, "views/x.erb"): "views/x.erb", "new/dir/file.txt": "new/dir/file.txt"} {
		if got, err := service.confine(dir, path); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestFilterServesTheHarnessBehindTheAppGates(t *testing.T) {
	service, _, dir := newTestService(t)
	app := testSnapshot(dir)

	// Another path or a web process without vibe is the app's.
	if _, passed := serve(service, app, httptest.NewRequest(http.MethodGet, "http://shop.test/cart", nil)); !passed {
		t.Fatal("a normal path did not pass through")
	}
	if _, passed := serve(service, app, httptest.NewRequest(http.MethodGet, "http://admin.shop.test"+config.VibePath+"/panel", nil)); !passed {
		t.Fatal("a web process without vibe answered the harness")
	}

	// The shell lives on the app's pages: the bare path and the old sign-in routes are gone.
	for _, gone := range []string{"", "/login", "/logout"} {
		if recorder, passed := serve(service, app, httptest.NewRequest(http.MethodGet, "http://shop.test"+config.VibePath+gone, nil)); passed || recorder.Code != http.StatusNotFound {
			t.Errorf("%q = %d", gone, recorder.Code)
		}
	}
	recorder, _ := serve(service, app, httptest.NewRequest(http.MethodGet, "http://shop.test"+config.VibePath+"/panel", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") || recorder.Header().Get("X-Frame-Options") != "SAMEORIGIN" || !strings.Contains(recorder.Body.String(), "<vibe-app>") {
		t.Fatalf("panel = %d %v", recorder.Code, recorder.Header())
	}
	if recorder, _ := serve(service, app, httptest.NewRequest(http.MethodGet, "http://shop.test"+config.VibePath+"/assets/shell.js", nil)); recorder.Code != http.StatusOK {
		t.Fatalf("shell.js = %d", recorder.Code)
	}

	// A page navigation asks the app for an uncompressed answer; a fetch keeps its encoding.
	page := httptest.NewRequest(http.MethodGet, "http://shop.test/cart", nil)
	page.Header.Set("Sec-Fetch-Dest", "document")
	page.Header.Set("Accept-Encoding", "gzip, br")
	page.Header.Set("If-None-Match", `W/"1"`)
	serve(service, app, page)
	if page.Header.Get("Accept-Encoding") != "" || page.Header.Get("If-None-Match") != "" {
		t.Fatalf("a page navigation kept %v", page.Header)
	}
	fetch := httptest.NewRequest(http.MethodGet, "http://shop.test/cart", nil)
	fetch.Header.Set("Accept-Encoding", "gzip")
	serve(service, app, fetch)
	if fetch.Header.Get("Accept-Encoding") != "gzip" {
		t.Fatal("a fetch lost Accept-Encoding")
	}

	// The harness vouches only for its own http_get, never for a visitor.
	if service.Authorizes(httptest.NewRequest(http.MethodGet, "http://shop.test/cart", nil), app) {
		t.Fatal("a visitor was authorized")
	}
}

func TestInjectDocksTheShellOntoPages(t *testing.T) {
	service, _, dir := newTestService(t)
	app := testSnapshot(dir)
	// dest "html" is a plain-http navigation: no Sec-Fetch-Dest, an Accept that wants HTML.
	respond := func(host, dest string, status int, contentType, encoding, body string) *http.Response {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/cart", nil)
		if dest == "html" {
			request.Header.Set("Accept", "text/html,application/xhtml+xml")
		} else if dest != "" {
			request.Header.Set("Sec-Fetch-Dest", dest)
		}
		response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}, "Content-Security-Policy": {"default-src 'self'"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
		if encoding != "" {
			response.Header.Set("Content-Encoding", encoding)
		}
		service.Inject(request, app, response)
		return response
	}
	read := func(response *http.Response) string {
		data, _ := io.ReadAll(response.Body)
		return string(data)
	}

	page := respond("shop.test", "document", http.StatusOK, "text/html; charset=utf-8", "", "<html><HEAD><title>x</title></HEAD><body>hi</body></html>")
	body := read(page)
	if body != "<html><HEAD><title>x</title>"+shellTag+"</HEAD><body>hi</body></html>" || page.Header.Get("Content-Length") != fmt.Sprint(len(body)) || page.ContentLength != int64(len(body)) {
		t.Fatalf("page = %q, length %s", body, page.Header.Get("Content-Length"))
	}
	if page.Header.Get("Cache-Control") != "no-store" || page.Header.Get("Content-Security-Policy") != "" {
		t.Fatalf("page headers = %v", page.Header)
	}
	if got := read(respond("shop.test", "document", http.StatusOK, "text/html", "", `<body class="x">hi`)); got != `<body class="x">`+shellTag+"hi" {
		t.Fatalf("no head = %q", got)
	}
	if got := read(respond("shop.test", "html", http.StatusOK, "text/html", "", "<head></head>")); got != "<head>"+shellTag+"</head>" {
		t.Fatalf("plain http navigation = %q", got)
	}
	if got := read(respond("shop.test", "document", http.StatusOK, "text/html", "", "<p>hi</p>")); got != "<p>hi</p>" {
		t.Fatalf("fragment = %q", got)
	}

	// Everything else passes untouched: a fetch, a frame, JSON, an error, a compressed page and a
	// web process without vibe.
	for name, response := range map[string]*http.Response{
		"fetch":   respond("shop.test", "", http.StatusOK, "text/html", "", "<head></head>"),
		"frame":   respond("shop.test", "iframe", http.StatusOK, "text/html", "", "<head></head>"),
		"json":    respond("shop.test", "document", http.StatusOK, "application/json", "", "<head></head>"),
		"error":   respond("shop.test", "document", http.StatusInternalServerError, "text/html", "", "<head></head>"),
		"gzip":    respond("shop.test", "document", http.StatusOK, "text/html", "gzip", "<head></head>"),
		"no vibe": respond("admin.shop.test", "document", http.StatusOK, "text/html", "", "<head></head>"),
	} {
		if got := read(response); got != "<head></head>" || response.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s = %q %v", name, got, response.Header)
		}
	}
}

func TestPostNeedsTheHarnessHeader(t *testing.T) {
	service, _, dir := newTestService(t)
	app := testSnapshot(dir)
	request := httptest.NewRequest(http.MethodPost, "http://shop.test"+config.VibePath+"/api/restart", strings.NewReader("{}"))
	if recorder, _ := serve(service, app, request); recorder.Code != http.StatusForbidden {
		t.Fatalf("POST without the header = %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "http://shop.test"+config.VibePath+"/api/restart", strings.NewReader("{}"))
	request.Header.Set("X-Dboss-Vibe", "1")
	request.Header.Set("Origin", "http://evil.test")
	if recorder, _ := serve(service, app, request); recorder.Code != http.StatusForbidden {
		t.Fatalf("POST from another origin = %d", recorder.Code)
	}
}

func TestToolsEditTheAppAndRecordTheChat(t *testing.T) {
	service, fake, dir := newTestService(t)
	h := service.harness("shop", "web")
	c := &call{s: service, app: "shop", web: "web", actor: "vibe:shop/web", harness: h}
	ctx := context.Background()
	if result, ok := service.runTool(ctx, c, "write_file", json.RawMessage(`{"path":"views/home.erb","content":"<h1>Hi</h1>\n<p>old</p>\n"}`)); !ok {
		t.Fatalf("write_file = %s", result)
	}
	if result, ok := service.runTool(ctx, c, "edit_file", json.RawMessage(`{"path":"views/home.erb","old_string":"old","new_string":"new"}`)); !ok {
		t.Fatalf("edit_file = %s", result)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "views/home.erb"))
	if string(data) != "<h1>Hi</h1>\n<p>new</p>\n" {
		t.Fatalf("file = %q", data)
	}
	if result, ok := service.runTool(ctx, c, "read_file", json.RawMessage(`{"path":"views/home.erb","offset":2}`)); !ok || !strings.Contains(result, "     2\t<p>new</p>") || strings.Contains(result, "<h1>") {
		t.Fatalf("read_file = %s", result)
	}
	if result, ok := service.runTool(ctx, c, "edit_file", json.RawMessage(`{"path":"views/home.erb","old_string":"missing","new_string":"x"}`)); ok || !strings.Contains(result, "not found") {
		t.Fatalf("a missing old_string = %s", result)
	}
	if result, ok := service.runTool(ctx, c, "read_file", json.RawMessage(`{"path":"../outside"}`)); ok || !strings.Contains(result, "outside the app folder") {
		t.Fatalf("an escaping read = %s", result)
	}
	if result, ok := service.runTool(ctx, c, "read_file", json.RawMessage(`{}`)); ok || !strings.Contains(result, `missing argument "path"`) {
		t.Fatalf("a call without its required argument = %s", result)
	}
	if result, ok := service.runTool(ctx, c, "run", json.RawMessage(`{"command":"bundle install"}`)); !ok || !strings.Contains(result, "exit code 0") {
		t.Fatalf("run = %s", result)
	}
	if !c.wrote {
		t.Fatal("a changing tool did not mark the turn")
	}
	if got := strings.Join(fake.methods(), ","); got != "exec:vibe:shop/web" {
		t.Fatalf("ops calls = %s", got)
	}
	if len(fake.audits) != 2 || fake.audits[0] != "vibe:shop/web vibe-write views/home.erb" || fake.audits[1] != "vibe:shop/web vibe-edit views/home.erb" {
		t.Fatalf("audits = %v", fake.audits)
	}
	entries := h.chat.Entries
	if len(entries) != 7 || entries[0].Tool != "write_file" || entries[0].Path != "views/home.erb" || !entries[0].OK || entries[3].OK {
		t.Fatalf("entries = %+v", entries)
	}
}

// fakeDeepSeek answers the first request with a tool call (split over two chunks, the way the API
// streams it) and the second with text, and records what it was sent.
func fakeDeepSeek(t *testing.T) (*httptest.Server, *[]completionRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		var body completionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, body)
		count := len(requests)
		mu.Unlock()
		if !body.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Make the header blue"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Reading. \",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"app.rb\\\"}\"}}]}}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Done, \"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"it says hi.\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	previous := deepseekURL
	deepseekURL = server.URL
	t.Cleanup(func() { deepseekURL = previous })
	return server, &requests
}

func TestChatTurnRunsToolsUntilTheModelAnswers(t *testing.T) {
	_, requests := fakeDeepSeek(t)
	service, _, dir := newTestService(t)
	if err := os.WriteFile(filepath.Join(dir, "app.rb"), []byte("puts 'hi'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := testSnapshot(dir)
	app.Web.DeepseekAPIKey = "sk-test"
	h := service.harness("shop", "web")
	_, events, done := h.subscribe()
	defer done()

	if err := service.startTurn(app, app.WebProcesses[0], "what does app.rb print?"); err != nil {
		t.Fatal(err)
	}
	if err := service.startTurn(app, app.WebProcesses[0], "again"); err == nil {
		t.Fatal("a second turn started while one runs")
	}
	deadline := time.After(5 * time.Second)
	var deltas strings.Builder
	for finished := false; !finished; {
		select {
		case ev := <-events:
			switch ev.Type {
			case "delta":
				deltas.WriteString(ev.Data.(map[string]string)["text"])
			case "turn":
				finished = !ev.Data.(turnState).Running
			}
		case <-deadline:
			t.Fatal("the turn did not end")
		}
	}
	if deltas.String() != "Reading. Done, it says hi." {
		t.Fatalf("streamed text = %q", deltas.String())
	}
	kinds := []string{}
	for _, entry := range h.chat.Entries {
		kinds = append(kinds, entry.Kind+":"+entry.Tool)
	}
	if strings.Join(kinds, ",") != "user:,assistant:,tool:read_file,assistant:" {
		t.Fatalf("entries = %v", kinds)
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %d", len(*requests))
	}
	second := (*requests)[1].Messages
	last := second[len(second)-1]
	if second[0].Role != "system" || !strings.Contains(second[0].Content, "app.rb") || last.Role != "tool" || last.ToolCallID != "call_1" || !strings.Contains(last.Content, "puts 'hi'") {
		t.Fatalf("second request = %+v", second)
	}
	if len((*requests)[0].Tools) != len(tools) {
		t.Fatalf("tools sent = %d", len((*requests)[0].Tools))
	}
	// The transcript is on disk for the next page load.
	if data, err := os.ReadFile(h.path); err != nil || !strings.Contains(string(data), "it says hi.") {
		t.Fatalf("transcript = %s %v", data, err)
	}
	message, err := service.complete(context.Background(), "sk-test", []Message{{Role: "user", Content: "diff"}})
	if err != nil || message != "Make the header blue" {
		t.Fatalf("complete = %q %v", message, err)
	}
}

func TestChatNeedsAKey(t *testing.T) {
	service, _, dir := newTestService(t)
	app := testSnapshot(dir)
	if err := service.startTurn(app, app.WebProcesses[0], "hi"); err == nil || !strings.Contains(err.Error(), "deepseek_api_key") {
		t.Fatalf("a turn without a key = %v", err)
	}
	// An unset $DEEPSEEK_API_KEY stays literal in the config and still means no key.
	app.Web.DeepseekAPIKey = "$DEEPSEEK_API_KEY"
	if err := service.startTurn(app, app.WebProcesses[0], "hi"); err == nil || !strings.Contains(err.Error(), "deepseek_api_key") {
		t.Fatalf("a turn with an unset reference = %v", err)
	}
	app.Web.DeepseekAPIKey = "sk-$abc"
	if chatKey(app) != "sk-$abc" {
		t.Fatal("a key containing a dollar sign was dropped")
	}
}

func TestRepairAndFitKeepRequestsValid(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: strings.Repeat("x", 500)},
		{Role: "tool", ToolCallID: "orphan", Content: "lost"},
		{Role: "user", Content: "two"},
	}
	repaired := repair(messages)
	ids := []string{}
	for _, message := range repaired {
		if message.Role == "tool" {
			ids = append(ids, message.ToolCallID)
		}
	}
	if strings.Join(ids, ",") != "b,a" {
		t.Fatalf("tool results = %v", ids)
	}
	fitted := fit(repaired, 100)
	if fitted[0].Role != "user" {
		t.Fatalf("fit cut mid-exchange: %+v", fitted[0])
	}
	for _, message := range fitted {
		if message.Role == "tool" && len(message.Content) > 200 {
			t.Fatal("old tool output was not stubbed")
		}
	}
}

func TestMCPServesTheToolsBehindItsToken(t *testing.T) {
	service, _, dir := newTestService(t)
	if err := os.WriteFile(filepath.Join(dir, "app.rb"), []byte("puts 'hi'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := testSnapshot(dir)
	token, err := service.Token("shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	rpc := func(path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://shop.test"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", "2025-06-18")
		request.RemoteAddr = "192.0.2.10:1"
		recorder := httptest.NewRecorder()
		service.MCPFilter(recorder, request, app, func() { t.Fatal("the MCP path passed through") })
		return recorder
	}
	if recorder := rpc(config.VibePath+"/mcp/wrong", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", recorder.Code)
	}
	list := rpc(config.VibePath+"/mcp/"+token, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"read_file"`) || !strings.Contains(list.Body.String(), `"http_get"`) {
		t.Fatalf("tools/list = %d %s", list.Code, list.Body.String())
	}
	called := rpc(config.VibePath+"/mcp/"+token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"app.rb"}}}`)
	body, _ := io.ReadAll(called.Body)
	if called.Code != http.StatusOK || !strings.Contains(string(body), "puts 'hi'") {
		t.Fatalf("tools/call = %d %s", called.Code, body)
	}
	entries := service.harness("shop", "web").chat.Entries
	if len(entries) != 1 || entries[0].Kind != KindExternal || entries[0].Tool != "read_file" {
		t.Fatalf("an MCP call did not show in the chat: %+v", entries)
	}
}

func TestDiffOnlyShowsListedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	service, _, dir := newTestService(t)
	app := testSnapshot(dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(query string) *httptest.ResponseRecorder {
		recorder, _ := serve(service, app, httptest.NewRequest(http.MethodGet, "http://shop.test"+config.VibePath+"/api/diff?"+query, nil))
		return recorder
	}
	if recorder := get("path=new.txt"); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "+hello") {
		t.Fatalf("diff = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := get("path=" + url.QueryEscape("/etc/passwd")); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unlisted path = %d", recorder.Code)
	}
	if recorder := get("path=new.txt&commit=deadbeef"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown commit = %d", recorder.Code)
	}
}

// Fez keeps a boolean attribute rendered as "" (presence is true) and drops only false, null and
// undefined, so `disabled={x ? 'disabled' : ”}` would never enable the control again.
func TestComponentsPassBooleansToBooleanAttributes(t *testing.T) {
	pattern := regexp.MustCompile(`\b(disabled|checked|selected|hidden|readonly|required)=\{[^}]*: ''\}`)
	files, err := fs.Glob(staticFS(), "fez/*.fez")
	if err != nil || len(files) == 0 {
		t.Fatalf("components = %v, %v", files, err)
	}
	for _, name := range files {
		data, err := fs.ReadFile(staticFS(), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllString(string(data), -1) {
			t.Errorf("%s: %s renders disabled=\"\" when false; pass a boolean", name, match)
		}
	}
}
