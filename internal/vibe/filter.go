package vibe

import (
	"bytes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

const (
	// panelCSP keeps the panel to its own origin; only the app's pages on that origin may frame it.
	panelCSP = "default-src 'self'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'self'; img-src 'self' data:; object-src 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'"
	// maxShellPage bounds the HTML page read whole to dock the shell; a larger one passes untouched.
	maxShellPage = 8 << 20
	shellTag     = `<script src="` + config.VibePath + `/assets/shell.js" defer></script>`
)

// route reports whether p is under the harness path and returns the rest after it.
func route(p string) (string, bool) {
	if p == config.VibePath {
		return "", true
	}
	return strings.CutPrefix(p, config.VibePath+"/")
}

// MCPFilter is the early proxy stage for the MCP endpoint, which carries its own token and so runs
// ahead of the app's gates. Every other harness path waits for Filter, behind them.
func (s *Service) MCPFilter(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled {
		next()
		return
	}
	rest, ok := route(r.URL.Path)
	if !ok || rest != "mcp" && !strings.HasPrefix(rest, "mcp/") {
		next()
		return
	}
	s.serveMCP(w, r, app, web, strings.TrimPrefix(strings.TrimPrefix(rest, "mcp"), "/"))
}

// Filter runs behind the app's own gates, so whoever the app lets in gets the harness with it. It
// answers the panel, its assets and its API; the bare harness path has no page, the shell lives on
// the app's pages. A page navigation to the app asks for a fresh, uncompressed answer, so Inject
// can read it and a cached copy without the shell is never reused.
func (s *Service) Filter(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled {
		next()
		return
	}
	rest, ok := route(r.URL.Path)
	if !ok {
		if document(r) {
			// The transport then asks for gzip itself and hands Inject the decoded body.
			r.Header.Del("Accept-Encoding")
			r.Header.Del("If-None-Match")
			r.Header.Del("If-Modified-Since")
		}
		next()
		return
	}
	switch {
	case rest == "panel":
		s.servePanel(w, r)
	case strings.HasPrefix(rest, "assets/"):
		s.serveAsset(w, r, strings.TrimPrefix(rest, "assets/"))
	case strings.HasPrefix(rest, "api/"):
		s.serveAPI(w, r, app, web, strings.TrimPrefix(rest, "api/"))
	default:
		http.NotFound(w, r)
	}
}

// Authorizes lets the harness's own http_get past the app's gates, so the agent can check any
// page it changed. Everyone else passes the gates like any visitor.
func (s *Service) Authorizes(r *http.Request, app supervisor.Snapshot) bool {
	web, ok := app.WebForHost(r.Host)
	return ok && web.Vibe.Enabled && internal(r, app, web)
}

// Inject docks the harness shell onto an HTML page the app answers to a page navigation: one
// script tag before </head>. A compressed, non-HTML, non-200 or very large answer, and an HTML
// fragment with neither head nor body, passes untouched. The app's CSP goes for that page, since
// it would refuse the shell and its panel.
func (s *Service) Inject(r *http.Request, app supervisor.Snapshot, response *http.Response) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled || !document(r) || response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.ContentLength > maxShellPage {
		return
	}
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType != "text/html" {
		return
	}
	page, err := io.ReadAll(io.LimitReader(response.Body, maxShellPage+1))
	if err != nil || len(page) > maxShellPage {
		// Hand on what was read and the rest, so the visitor still gets the page or the error.
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(page), response.Body), response.Body}
		return
	}
	_ = response.Body.Close()
	docked, ok := withShell(page)
	if !ok {
		response.Body = io.NopCloser(bytes.NewReader(page))
		return
	}
	page = docked
	response.Body = io.NopCloser(bytes.NewReader(page))
	response.ContentLength = int64(len(page))
	response.Header.Set("Content-Length", strconv.Itoa(len(page)))
	response.Header.Set("Cache-Control", "no-store")
	response.Header.Del("ETag")
	response.Header.Del("Content-Security-Policy")
	response.Header.Del("Content-Security-Policy-Report-Only")
}

// document reports whether r is a page navigation, the only request the shell docks onto: not a
// fetch, not a frame, not a HEAD. Browsers send Sec-Fetch-Dest to secure origins only, so on plain
// http (a dev session on lvh.me) a GET that accepts HTML counts; withShell then skips fragments,
// and shell.js stays out of frames by itself.
func document(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// withShell puts the shell's script tag before </head>, else right after <body ...>. A page with
// neither is a fragment and reports false.
func withShell(page []byte) ([]byte, bool) {
	lower := asciiLower(page)
	at := bytes.Index(lower, []byte("</head>"))
	if at < 0 {
		if body := bytes.Index(lower, []byte("<body")); body >= 0 {
			if end := bytes.IndexByte(page[body:], '>'); end >= 0 {
				at = body + end + 1
			}
		}
	}
	if at < 0 {
		return nil, false
	}
	return slices.Concat(page[:at], []byte(shellTag), page[at:]), true
}

// asciiLower lowercases only A-Z, so every index into the result is an index into page.
func asciiLower(page []byte) []byte {
	lower := make([]byte, len(page))
	for i, c := range page {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return lower
}

// servePanel answers the shell's panel, which only the app's own pages may frame.
func (s *Service) servePanel(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.assets, "panel.html")
	if err != nil {
		http.Error(w, "harness panel missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", panelCSP)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Referrer-Policy", "same-origin")
	_, _ = w.Write(data)
}

// serveAsset answers the harness's own files, and the console's fez runtime and stylesheet under
// console/.
func (s *Service) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	source := s.assets
	if rest, ok := strings.CutPrefix(name, "console/"); ok {
		source, name = s.console, rest
	}
	if name == "" || !fs.ValidPath(name) || source == nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(source, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
