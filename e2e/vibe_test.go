//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The vibe demo serves the harness behind its own password, while the site itself stays public;
// the MCP endpoint behind its token edits and reads the app through dboss, and the start step made
// the folder its own repository, so Changes and Reset see exactly the harness's edits.
func TestVibeHarness(t *testing.T) {
	ensureRunning(t, "vibe")
	expectBody(t, serving(t, "vibe.lvh.me", "/"), "Corner Bakery")

	page := html(t, "vibe.lvh.me", "/_dboss_/vibe")
	expectStatus(t, page, http.StatusUnauthorized)
	expectBody(t, page, "vibe vibe", `action="/_dboss_/vibe/login"`)

	form := url.Values{"password": {"vibe"}, "to": {"/_dboss_/vibe"}}
	login := call{method: http.MethodPost, host: "vibe.lvh.me", path: "/_dboss_/vibe/login", body: []byte(form.Encode()), header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}.do(t)
	expectStatus(t, login, http.StatusSeeOther)
	cookie, _, _ := strings.Cut(login.Header.Get("Set-Cookie"), ";")
	if !strings.HasPrefix(cookie, "dboss_vibe=") {
		t.Fatalf("vibe login set %q", login.Header.Get("Set-Cookie"))
	}
	signed := map[string]string{"Cookie": cookie}

	harness := call{host: "vibe.lvh.me", path: "/_dboss_/vibe", header: signed}.do(t)
	expectStatus(t, harness, http.StatusOK)
	expectBody(t, harness, "<vibe-app>")
	if harness.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("harness page headers = %v", harness.Header)
	}
	expectStatus(t, call{host: "vibe.lvh.me", path: "/_dboss_/vibe/assets/console/fez.min.js", header: signed}.do(t), http.StatusOK)

	state := call{host: "vibe.lvh.me", path: "/_dboss_/vibe/api/state", header: signed}.do(t)
	expectStatus(t, state, http.StatusOK)
	var info struct {
		App    string `json:"app"`
		MCPURL string `json:"mcp_url"`
		Chat   bool   `json:"chat"`
	}
	if err := json.Unmarshal([]byte(state.Body), &info); err != nil || info.App != "vibe" || info.Chat {
		t.Fatalf("state = %s, %v", state.Body, err)
	}
	endpoint, err := url.Parse(info.MCPURL)
	if err != nil || !strings.HasPrefix(endpoint.Path, "/_dboss_/vibe/mcp/") {
		t.Fatalf("mcp url = %q", info.MCPURL)
	}

	rpc := func(path, body string) reply {
		return call{method: http.MethodPost, host: "vibe.lvh.me", path: path, body: []byte(body), header: map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-06-18"}}.do(t)
	}
	expectStatus(t, rpc("/_dboss_/vibe/mcp/wrong", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), http.StatusUnauthorized)
	list := rpc(endpoint.Path, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	expectStatus(t, list, http.StatusOK)
	expectBody(t, list, `"edit_file"`, `"http_get"`)
	read := rpc(endpoint.Path, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"dboss.yaml"}}}`)
	expectBody(t, read, "vibe:")
	page2 := rpc(endpoint.Path, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"http_get","arguments":{"path":"/"}}}`)
	expectBody(t, page2, "200 OK", "Corner Bakery")
	escape := rpc(endpoint.Path, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"../../dboss.yaml"}}}`)
	expectBody(t, escape, "outside the app folder", `"isError":true`)

	changes := func() string {
		r := call{host: "vibe.lvh.me", path: "/_dboss_/vibe/api/changes", header: signed}.do(t)
		expectStatus(t, r, http.StatusOK)
		return r.Body
	}
	if body := changes(); !strings.Contains(body, `"branch":"main"`) || strings.Contains(body, "notes.txt") {
		t.Fatalf("changes before the edit = %s", body)
	}
	write := rpc(endpoint.Path, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"notes.txt","content":"hello\n"}}}`)
	expectBody(t, write, "notes.txt")
	expectBody(t, reply{Body: changes()}, `"path":"notes.txt"`)
	post := map[string]string{"Cookie": cookie, "X-Dboss-Vibe": "1", "Content-Type": "application/json"}
	reset := call{method: http.MethodPost, host: "vibe.lvh.me", path: "/_dboss_/vibe/api/reset", body: []byte("{}"), header: post}.do(t)
	expectStatus(t, reset, http.StatusOK)
	if body := changes(); strings.Contains(body, "notes.txt") {
		t.Fatalf("changes after the reset = %s", body)
	}

	// bun sets no vibe: the path is the app's own, so the request reaches bun and gets its 404.
	expectStatus(t, get(t, "bun.lvh.me", "/_dboss_/vibe"), http.StatusNotFound)
}
