package console

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/config"
	"dboss/internal/httpx"
	"dboss/internal/pages"
	"dboss/internal/secret"
	"dboss/internal/throttle"
)

const (
	authCallbackPath  = "/authcog"
	authStateCookie   = "dboss_console_auth_state"
	authSessionCookie = "dboss_console_session"
	authAudience      = "console"
	// `dboss login` mints a one-time link for a local operator; the session it creates belongs
	// to cliEmail, which AuthCog can never vouch for.
	cliLoginPath = "/login"
	cliEmail     = "cli@localhost"
	cliTokenTTL  = 3 * time.Minute
	// A management.listen address signs in with management.password; the session belongs to
	// passwordEmail, which AuthCog can never vouch for either.
	passwordPath    = "/password"
	passwordCookie  = "dboss_console_password"
	passwordEmail   = "password@console"
	passwordMaxForm = 4 << 10
)

// directKey marks a request that arrived on a management.listen address.
type directKey struct{}

func isDirect(r *http.Request) bool {
	direct, _ := r.Context().Value(directKey{}).(bool)
	return direct
}

func withDirect(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), directKey{}, true))
}

type authSession = authcog.Session

// authenticator is the console's side of the shared AuthCog flow: admins only, plus the
// one-time `dboss login` tokens and, on management.listen addresses, management.password.
type authenticator struct {
	flow         *authcog.Flow
	gate         authcog.Gate
	admins       map[string]bool
	mu           sync.Mutex
	cliTokens    map[string]cliToken
	local        bool
	localSession authSession // the one local session a hand-run or dev session hands out
	hostPages    func() string
	password     string
	// passwordGate signs password sessions; its audience carries a digest of the password, so
	// changing it signs everyone out.
	passwordGate authcog.Gate
	passwords    *throttle.Throttle
}

func consoleAuthenticator(flow *authcog.Flow, cfg config.Config, hostPages func() string, passwords *throttle.Throttle) (*authenticator, error) {
	management := cfg.Management
	// A dev session (running an app from its own folder) and a hand-run host in a terminal both
	// trust the machine's own loopback, so neither sends the operator through a token.
	local := cfg.Dev() || cfg.Local
	hosts := make(map[string]bool, len(management.Host))
	for _, host := range management.Host {
		hosts[strings.ToLower(host)] = true
	}
	auth := &authenticator{flow: flow, admins: map[string]bool{}, local: local, hostPages: hostPages, password: management.Password, passwords: passwords}
	if management.Password != "" {
		digest := sha256.Sum256([]byte(management.Password))
		auth.passwordGate = authcog.Gate{
			Audience:      authAudience + "-password:" + hex.EncodeToString(digest[:8]),
			SessionCookie: passwordCookie,
			TTL:           cfg.Defaults.SessionTTL.Value(),
		}
	}
	for _, email := range management.Admins {
		auth.admins[strings.ToLower(email)] = true
	}
	auth.gate = authcog.Gate{
		Audience:      authAudience,
		Realm:         cfg.AuthCogRealm,
		Hosts:         func(host string) bool { return hosts[host] },
		CallbackPath:  authCallbackPath,
		StateCookie:   authStateCookie,
		SessionCookie: authSessionCookie,
		TTL:           cfg.Defaults.SessionTTL.Value(),
		Allow:         func(email string) bool { return auth.admins[email] },
	}
	if local {
		// One session for the whole run, not a cookie minted per request: the console reads its
		// CSRF token once at boot and polls for the rest of the session, so a token that changed
		// under it would start failing every mutating call.
		csrf, err := authcog.RandomToken()
		if err != nil {
			return nil, err
		}
		// It is never signed into a cookie, so its expiry is only there to outlive the run.
		auth.localSession = authSession{Email: cliEmail, CSRF: csrf, Audience: authAudience, ExpiresAt: time.Now().AddDate(10, 0, 0).Unix()}
	}
	return auth, nil
}

func (a *authenticator) authenticate(w http.ResponseWriter, r *http.Request) (authSession, bool) {
	if r.URL.Path == authCallbackPath {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return authSession{}, false
		}
		a.flow.Callback(w, r, a.gate)
		return authSession{}, false
	}
	if r.URL.Path == cliLoginPath {
		a.cliLogin(w, r)
		return authSession{}, false
	}
	if session, ok := a.validSession(r); ok {
		return session, true
	}
	if isDirect(r) {
		return a.passwordSession(w, r)
	}
	if a.local && loopbackPeer(r) {
		// A hand-run or dev session answering its own machine is the operator's own terminal, so
		// sending them to `dboss login` buys nothing; hand them the local session directly.
		return a.localSession, true
	}
	if strings.HasPrefix(r.URL.Path, "/ui/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"authentication required"}`+"\n")
		return authSession{}, false
	}
	if loopbackHost(r.Host) {
		// AuthCog has no destination for a loopback name, so the only way in here is the link
		// from `dboss login`.
		pages.Page{Name: pages.Login}.Write(w, a.hostPages())
		return authSession{}, false
	}
	a.flow.Start(w, r, a.gate)
	return authSession{}, false
}

// passwordSession signs a management.listen request in with management.password: the session it
// already holds, the form post, or the password page. Every check books a slot in the per-IP
// throttle, a passing one too, so guessing is slow.
func (a *authenticator) passwordSession(w http.ResponseWriter, r *http.Request) (authSession, bool) {
	if r.URL.Path == passwordPath && r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, passwordMaxForm)
		to := authcog.SafeRedirect(r.PostFormValue("to"), passwordPath)
		slot, ok := a.passwords.Reserve(httpx.ClientIP(r))
		if !ok {
			seconds := int(math.Ceil(slot.Wait.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			a.passwordPage(w, r, to, http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %d seconds.", seconds))
			return authSession{}, false
		}
		time.Sleep(slot.Wait)
		if !secret.Matches(a.password, r.PostFormValue("password")) {
			a.passwordPage(w, r, to, http.StatusUnauthorized, "Wrong password.")
			return authSession{}, false
		}
		if err := a.flow.SetSession(w, r, a.passwordGate, passwordEmail); err != nil {
			http.Error(w, "authentication unavailable", http.StatusInternalServerError)
			return authSession{}, false
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return authSession{}, false
	}
	if session, ok := a.flow.Session(r, a.passwordGate); ok {
		return session, true
	}
	if strings.HasPrefix(r.URL.Path, "/ui/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"authentication required"}`+"\n")
		return authSession{}, false
	}
	a.passwordPage(w, r, authcog.SafeRedirect(r.URL.RequestURI(), passwordPath), http.StatusUnauthorized, "")
	return authSession{}, false
}

// passwordPage answers status with the password form, and problem above it, for a browser and
// a bare status otherwise.
func (a *authenticator) passwordPage(w http.ResponseWriter, r *http.Request, to string, status int, problem string) {
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Error(w, "password required", status)
		return
	}
	action := pages.PasswordForm(passwordPath, to, problem)
	pages.Page{Name: pages.Password, App: "dboss console", Status: status, Action: action}.Write(w, a.hostPages())
}

// loopbackHost reports whether rawHost names this machine: localhost or a loopback address,
// with or without a port.
func loopbackHost(rawHost string) bool {
	host := rawHost
	if parsedHost, _, err := net.SplitHostPort(rawHost); err == nil {
		host = parsedHost
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackPeer reports whether the request's own TCP peer is this machine. It reads RemoteAddr
// and never a forwarded-for header, so a request from off-box cannot claim to be local; a
// reverse proxy on the same host, however, makes every request look local.
func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.Trim(r.RemoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cliToken is one pending `dboss login` link, spent on first use.
type cliToken struct {
	expiresAt time.Time
}

// issueCLIToken returns a fresh single-use login token that expires after cliTokenTTL.
func (a *authenticator) issueCLIToken() (string, error) {
	return a.issueToken(cliTokenTTL)
}

func (a *authenticator) issueToken(ttl time.Duration) (string, error) {
	token, err := authcog.RandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cliTokens == nil {
		a.cliTokens = map[string]cliToken{}
	}
	for key, pending := range a.cliTokens {
		if !pending.expiresAt.After(now) {
			delete(a.cliTokens, key)
		}
	}
	a.cliTokens[token] = cliToken{expiresAt: now.Add(ttl)}
	return token, nil
}

func (a *authenticator) cliLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Query().Get("token")
	a.mu.Lock()
	pending, ok := a.cliTokens[token]
	delete(a.cliTokens, token)
	a.mu.Unlock()
	if token == "" || !ok || !pending.expiresAt.After(time.Now()) {
		http.Error(w, "login link is invalid or expired; run dboss login again", http.StatusBadRequest)
		return
	}
	if err := a.setSessionCookie(w, r, cliEmail); err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, r *http.Request, email string) error {
	return a.flow.SetSession(w, r, a.gate, email)
}

// validSession re-checks the admin list on every request, so removing an email ends its sessions.
func (a *authenticator) validSession(r *http.Request) (authSession, bool) {
	session, ok := a.flow.Session(r, a.gate)
	if !ok {
		return authSession{}, false
	}
	if email := strings.ToLower(session.Email); !a.admins[email] && email != cliEmail {
		return authSession{}, false
	}
	return session, true
}

func (a *authenticator) validCSRF(r *http.Request, session authSession) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRF)) != 1 {
		return false
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme != authcog.Scheme(r) || !strings.EqualFold(origin.Host, r.Host) {
		return false
	}
	return true
}

func (a *authenticator) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	a.flow.ClearSession(w, r, a.gate)
	if a.password != "" {
		a.flow.ClearSession(w, r, a.passwordGate)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, session authSession) {
	if !h.requireCSRF(w, r, session) {
		return
	}
	h.auth.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request, session authSession) bool {
	if h.auth.validCSRF(r, session) {
		return true
	}
	writeError(w, http.StatusForbidden, "invalid CSRF token")
	return false
}
