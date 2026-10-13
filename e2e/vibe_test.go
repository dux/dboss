//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The vibe demo sits behind the app's own password, and whoever passes it gets the site with the
// vibe shell docked on every page; the MCP endpoint behind its token edits and reads the app through
// dboss, and the start step made the folder its own repository, so Changes and Reset see exactly the
// harness's edits.
func TestVibeHarness(t *testing.T) {
	ensureRunning(t, "vibe")
	expectStatus(t, html(t, "vibe.lvh.me", "/_dboss_/vibe/panel"), http.StatusUnauthorized)
	signed := passwordLogin(t, "vibe.lvh.me", "vibe")

	navigate := map[string]string{"Cookie": signed["Cookie"], "Sec-Fetch-Dest": "document", "Accept-Encoding": "gzip"}
	page := servingCall(t, call{host: "vibe.lvh.me", path: "/", header: navigate})
	expectBody(t, page, "Corner Bakery", `<script src="/_dboss_/vibe/assets/shell.js" defer></script></head>`)
	if page.Header.Get("Cache-Control") != "no-store" || page.Header.Get("Content-Encoding") != "" {
		t.Fatalf("page headers = %v", page.Header)
	}
	if fetched := (call{host: "vibe.lvh.me", path: "/", header: signed}).do(t); strings.Contains(fetched.Body, "shell.js") {
		t.Fatal("a fetch got the shell")
	}
	expectStatus(t, call{host: "vibe.lvh.me", path: "/_dboss_/vibe", header: signed}.do(t), http.StatusNotFound)

	panel := call{host: "vibe.lvh.me", path: "/_dboss_/vibe/panel", header: signed}.do(t)
	expectStatus(t, panel, http.StatusOK)
	expectBody(t, panel, "<vibe-app>")
	if panel.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("panel headers = %v", panel.Header)
	}
	expectStatus(t, call{host: "vibe.lvh.me", path: "/_dboss_/vibe/assets/shell.js", header: signed}.do(t), http.StatusOK)
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
	post := map[string]string{"Cookie": signed["Cookie"], "X-Dboss-Vibe": "1", "Content-Type": "application/json"}
	reset := call{method: http.MethodPost, host: "vibe.lvh.me", path: "/_dboss_/vibe/api/reset", body: []byte("{}"), header: post}.do(t)
	expectStatus(t, reset, http.StatusOK)
	if body := changes(); strings.Contains(body, "notes.txt") {
		t.Fatalf("changes after the reset = %s", body)
	}

	// bun sets no vibe: the path is the app's own, so the request reaches bun and gets its 404.
	expectStatus(t, get(t, "bun.lvh.me", "/_dboss_/vibe"), http.StatusNotFound)
}
