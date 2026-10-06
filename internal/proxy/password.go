package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/pages"
	"dboss/internal/supervisor"
)

const (
	passwordPath    = "/.well-known/dboss/password"
	passwordCookie  = "dboss_password"
	passwordMaxForm = 4 << 10
	// passwordSpacing is the least time between two password checks from one client IP, across
	// every app, so guessing is slow; parallel requests queue instead of slipping through.
	passwordSpacing = 3 * time.Second
	// passwordMaxWait caps that queue: a check that would wait longer answers 429 at once, so a
	// flood cannot pile up waiting connections.
	passwordMaxWait = 30 * time.Second
)

// passwordThrottle hands out password check slots per client IP. It lives in memory only, so a
// restart starts every IP fresh. now is injectable for tests.
type passwordThrottle struct {
	mu      sync.Mutex
	next    map[string]time.Time
	spacing time.Duration
	now     func() time.Time
}

func newPasswordThrottle() *passwordThrottle {
	return &passwordThrottle{next: map[string]time.Time{}, spacing: passwordSpacing, now: time.Now}
}

// reserve books the IP's next check slot and returns how long to wait for it, or false with the
// wait when the queue is past passwordMaxWait.
func (t *passwordThrottle) reserve(ip string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	slot := now
	if next, ok := t.next[ip]; ok && next.After(now) {
		slot = next
	}
	wait := slot.Sub(now)
	if wait > passwordMaxWait {
		return wait, false
	}
	t.next[ip] = slot.Add(t.spacing)
	return wait, true
}

// sweep forgets the IPs whose next slot has passed; they would get an immediate check anyway.
func (t *passwordThrottle) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for ip, next := range t.next {
		if !next.After(now) {
			delete(t.next, ip)
		}
	}
}

// passwordGate asks for the web process's shared password on a dboss page and keeps the visitor
// in with a signed cookie lasting session_ttl. The audience carries a digest of the configured
// value, so changing the password signs everyone out, and a cookie for one web process never
// opens another.
func (h *Handler) passwordGate(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, next func()) {
	web, _, password := gates(r, app)
	if password == "" || h.exempt(r, app) {
		next()
		return
	}
	digest := sha256.Sum256([]byte(password))
	gate := authcog.Gate{
		Audience:      "password:" + app.Name + "/" + web + ":" + hex.EncodeToString(digest[:8]),
		SessionCookie: passwordCookie,
		TTL:           app.Web.SessionTTL.Value(),
	}
	if r.URL.Path == passwordPath && r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, passwordMaxForm)
		to := authcog.SafeRedirect(r.PostFormValue("to"), passwordPath)
		wait, ok := h.passwords.reserve(clientIP(r, h.cfg.Proxy.Cloudflare))
		if !ok {
			seconds := int(math.Ceil(wait.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			h.passwordPage(w, r, app, to, http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %d seconds.", seconds))
			return
		}
		time.Sleep(wait)
		if secretMatches(password, r.PostFormValue("password")) {
			if err := h.signin.SetSession(w, r, gate, ""); err != nil {
				http.Error(w, "could not start a session", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		h.passwordPage(w, r, app, to, http.StatusUnauthorized, "Wrong password.")
		return
	}
	if _, ok := h.signin.Session(r, gate); ok {
		next()
		return
	}
	h.passwordPage(w, r, app, authcog.SafeRedirect(r.URL.RequestURI(), passwordPath), http.StatusUnauthorized, "")
}

// passwordPage answers status with the password form, and problem above it, for a browser and
// a bare status otherwise.
func (h *Handler) passwordPage(w http.ResponseWriter, r *http.Request, app supervisor.Snapshot, to string, status int, problem string) {
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Error(w, "password required", status)
		return
	}
	action := `<form class="password" method="post" action="` + passwordPath + `">`
	if problem != "" {
		action += `<p class="error">` + html.EscapeString(problem) + `</p>`
	}
	action += `<input type="password" name="password" aria-label="Password" placeholder="Password" autocomplete="off" autofocus required>` +
		`<input type="hidden" name="to" value="` + html.EscapeString(to) + `">` +
		`<button type="submit">Continue</button></form>`
	pages.Page{Name: pages.Password, App: app.Name, Status: status, Action: action}.Write(w, h.pageDirs(app)...)
}
