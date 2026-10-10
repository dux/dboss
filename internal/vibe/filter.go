package vibe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/httpx"
	"dboss/internal/pages"
	"dboss/internal/secret"
	"dboss/internal/supervisor"
)

const (
	sessionCookie = "dboss_vibe"
	maxLoginForm  = 4 << 10
	// pageCSP keeps the harness page to its own origin; the frame loads the app on the same one.
	pageCSP = "default-src 'self'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; frame-src 'self'; img-src 'self' data:; object-src 'none'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'"
)

// route reports whether p is under the harness path and returns the rest after it.
func route(p string) (string, bool) {
	if p == config.VibePath {
		return "", true
	}
	return strings.CutPrefix(p, config.VibePath+"/")
}

// Filter is the early proxy stage. It answers the harness path of a web process with vibe and
// lets every other request through. MCP carries its own token; everything else needs the vibe
// session (or no password, in a dev session).
func (s *Service) Filter(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled {
		next()
		return
	}
	rest, ok := route(r.URL.Path)
	if !ok {
		next()
		return
	}
	if rest == "mcp" || strings.HasPrefix(rest, "mcp/") {
		s.serveMCP(w, r, app, web, strings.TrimPrefix(strings.TrimPrefix(rest, "mcp"), "/"))
		return
	}
	if rest == "login" {
		s.login(w, r, app, web)
		return
	}
	if !s.signedIn(r, app, web) {
		if rest == "" && r.Method == http.MethodGet {
			s.loginPage(w, r, app, web, http.StatusUnauthorized, "", safeTarget(r.URL.RequestURI()))
			return
		}
		http.Error(w, "vibe password required", http.StatusUnauthorized)
		return
	}
	switch {
	case rest == "":
		s.servePage(w, r)
	case rest == "logout":
		s.flow.ClearSession(w, r, s.gate(app, web))
		http.Redirect(w, r, config.VibePath, http.StatusSeeOther)
	case strings.HasPrefix(rest, "assets/"):
		s.serveAsset(w, r, strings.TrimPrefix(rest, "assets/"))
	case strings.HasPrefix(rest, "api/"):
		s.serveAPI(w, r, app, web, strings.TrimPrefix(rest, "api/"))
	default:
		http.NotFound(w, r)
	}
}

// Authorizes lets the signed-in owner of a web process's harness past the app's own password,
// basic_auth and auth gates, so the framed app opens. A harness with no password (a dev session)
// vouches for nobody: the app's gates apply as usual.
func (s *Service) Authorizes(r *http.Request, app supervisor.Snapshot) bool {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled {
		return false
	}
	if internal(r, app, web) {
		return true
	}
	if web.Vibe.Password == "" {
		return false
	}
	_, ok = s.flow.Session(r, s.gate(app, web))
	return ok
}

// FrameHeaders drops X-Frame-Options and the CSP frame-ancestors directive from app responses to
// the harness owner, so an app that refuses framing still renders in the harness. Every other
// visitor gets the app's headers unchanged.
func (s *Service) FrameHeaders(r *http.Request, app supervisor.Snapshot, header http.Header) {
	web, ok := app.WebForHost(r.Host)
	if !ok || !web.Vibe.Enabled || !s.signedIn(r, app, web) {
		return
	}
	header.Del("X-Frame-Options")
	for _, name := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		values := header.Values(name)
		if len(values) == 0 {
			continue
		}
		header.Del(name)
		for _, value := range values {
			if kept := withoutFrameAncestors(value); kept != "" {
				header.Add(name, kept)
			}
		}
	}
}

// withoutFrameAncestors removes the frame-ancestors directive from one CSP header value.
func withoutFrameAncestors(policy string) string {
	var kept []string
	for _, directive := range strings.Split(policy, ";") {
		directive = strings.TrimSpace(directive)
		if directive == "" || strings.EqualFold(strings.Fields(directive)[0], "frame-ancestors") {
			continue
		}
		kept = append(kept, directive)
	}
	return strings.Join(kept, "; ")
}

// gate is the vibe session of one web process. The audience carries a digest of the password, so
// changing it signs everyone out, and a session for one harness never opens another.
func (s *Service) gate(app supervisor.Snapshot, web supervisor.WebProcessSnapshot) authcog.Gate {
	digest := sha256.Sum256([]byte(web.Vibe.Password))
	return authcog.Gate{
		Audience:      "vibe:" + app.Name + "/" + web.Name + ":" + hex.EncodeToString(digest[:8]),
		SessionCookie: sessionCookie,
		TTL:           app.Web.SessionTTL.Value(),
	}
}

func (s *Service) signedIn(r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot) bool {
	if web.Vibe.Password == "" {
		return true
	}
	_, ok := s.flow.Session(r, s.gate(app, web))
	return ok
}

// login checks the vibe password. Every check books a slot in the per-IP throttle, so the password
// cannot be guessed fast. A GET (a reload or Back on a failed attempt's page) goes to the harness,
// which asks again when there is no session.
func (s *Service) login(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, config.VibePath, http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginForm)
	to := safeTarget(r.PostFormValue("to"))
	slot, ok := s.logins.Reserve(httpx.ClientIP(r, s.cloudflare))
	if !ok {
		seconds := int(math.Ceil(slot.Wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		s.loginPage(w, r, app, web, http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %d seconds.", seconds), to)
		return
	}
	time.Sleep(slot.Wait)
	if web.Vibe.Password == "" || !secret.Matches(web.Vibe.Password, r.PostFormValue("password")) {
		s.loginPage(w, r, app, web, http.StatusUnauthorized, "Wrong password.", to)
		return
	}
	s.logins.Release(slot)
	if err := s.flow.SetSession(w, r, s.gate(app, web), ""); err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// safeTarget keeps the post-sign-in redirect inside the harness and off the sign-in routes.
func safeTarget(target string) string {
	if rest, ok := route(strings.SplitN(target, "?", 2)[0]); !ok || rest == "login" || rest == "logout" || strings.Contains(target, "//") || strings.Contains(target, "\\") {
		return config.VibePath
	}
	return target
}

func (s *Service) loginPage(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, web supervisor.WebProcessSnapshot, status int, problem, to string) {
	w.Header().Set("Cache-Control", "no-store")
	action := `<form class="password" method="post" action="` + config.VibePath + `/login">`
	if problem != "" {
		action += `<p class="error">` + html.EscapeString(problem) + `</p>`
	}
	action += `<input type="password" name="password" aria-label="Password" placeholder="Password" autocomplete="current-password" autofocus required>` +
		`<input type="hidden" name="to" value="` + html.EscapeString(to) + `">` +
		`<button type="submit">Open</button></form>`
	pages.Page{Name: pages.Vibe, App: app.Name, Status: status, Action: action}.Write(w, app.Pages, s.ops.HostPages())
}

// servePage answers the harness page itself, which must never be framed.
func (s *Service) servePage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		http.Error(w, "harness page missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Header().Set("X-Frame-Options", "DENY")
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
