//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRoutesByHostWithLayeredEnv(t *testing.T) {
	r := serving(t, "bun.lvh.me", "/")
	// .env beats env: in dboss.yaml, SOURCE only comes from the config, PORT is always dboss's.
	expectBody(t, r, "Hello from Bun", "PROC_TYPE=web", "GREETING=from-dotenv", "SOURCE=config")
	if strings.Contains(r.Body, "PORT=unset") {
		t.Fatal("PORT was not injected")
	}
}

func TestInstancesShareTheLoad(t *testing.T) {
	ensureRunning(t, "bun")
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		body := serving(t, "bun.lvh.me", "/").Body
		for _, instance := range []string{"PROC_INSTANCE=1", "PROC_INSTANCE=2"} {
			if strings.Contains(body, instance) {
				seen[instance] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("count: 2 served from %v, want both instances", seen)
	}
}

func TestSecondWebProcessOwnsItsHost(t *testing.T) {
	expectBody(t, servingCall(t, call{host: "admin.bun.lvh.me", path: "/", header: passwordLogin(t, "admin.bun.lvh.me", "demo")}), "PROC_TYPE=admin")
}

// Only the admin web process sets a password; the app's other web process stays open.
func TestPasswordGuardsOneWebProcess(t *testing.T) {
	page := html(t, "admin.bun.lvh.me", "/")
	expectStatus(t, page, http.StatusUnauthorized)
	expectBody(t, page, "bun is protected", `name="password"`)
	expectStatus(t, get(t, "admin.bun.lvh.me", "/"), http.StatusUnauthorized)
	wrong := passwordPost(t, "admin.bun.lvh.me", "nope")
	expectStatus(t, wrong, http.StatusUnauthorized)
	expectBody(t, wrong, "Wrong password.")
	expectBody(t, serving(t, "bun.lvh.me", "/"), "PROC_TYPE=web")
}

func TestUnknownHostGetsNotFoundPage(t *testing.T) {
	r := html(t, "nope.lvh.me", "/")
	expectStatus(t, r, http.StatusNotFound)
	expectBody(t, r, "<html")
}

func TestStaticFilesComeFromDisk(t *testing.T) {
	r := get(t, "bun.lvh.me", "/dboss.svg")
	expectStatus(t, r, http.StatusOK)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	// /assets/ is the default immutable prefix; sinatra never starts for it (autostart: false).
	css := get(t, "sinatra.lvh.me", "/assets/app.css")
	expectStatus(t, css, http.StatusOK)
	if !strings.Contains(css.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("cache control %q", css.Header.Get("Cache-Control"))
	}
}

func TestStoppedAppStillServesStaticAndWakesOnRequest(t *testing.T) {
	ensureRunning(t, "bun")
	api(t, "stop", map[string]any{"app": "bun"}, nil)
	waitState(t, "bun", "stopped")
	expectStatus(t, get(t, "bun.lvh.me", "/dboss.svg"), http.StatusOK)
	// The first request wakes the app and gets the starting page; the app then serves.
	expectStatus(t, html(t, "bun.lvh.me", "/"), http.StatusServiceUnavailable)
	expectBody(t, serving(t, "bun.lvh.me", "/"), "Hello from Bun")
}

func TestDenyListRefusesScannerPaths(t *testing.T) {
	for _, path := range []string{"/wp-login.php", "/.env", "/wp-admin/setup", "/.git/config"} {
		expectStatus(t, get(t, "bun.lvh.me", path), http.StatusForbidden)
	}
	// The counters land with the next batch flush of the host database.
	client := consoleClient(t)
	eventually(t, 10*time.Second, func() error {
		r := call{addr: host.console, path: "/ui/log/blocked", client: client}.do(t)
		if r.Status != http.StatusOK || !strings.Contains(r.Body, "/wp-login.php") {
			return fmt.Errorf("blocked page %d: %.200s", r.Status, r.Body)
		}
		return nil
	})
}

func TestPublicHealthEndpoint(t *testing.T) {
	ensureRunning(t, "bun")
	expectStatus(t, get(t, "bun.lvh.me", "/.well-known/dboss/health"), http.StatusOK)
	// A stopped button app cannot be woken by a request, so it is not serving.
	if status(t, "button").State == "stopped" {
		expectStatus(t, get(t, "button.lvh.me", "/.well-known/dboss/health"), http.StatusServiceUnavailable)
	}
}

func TestMaxBodyRefusesLargeRequests(t *testing.T) {
	ensureRunning(t, "bun")
	large := call{method: http.MethodPost, host: "bun.lvh.me", path: "/notify", body: bytes.Repeat([]byte("x"), 2<<20)}.do(t)
	expectStatus(t, large, http.StatusRequestEntityTooLarge)
	small := call{method: http.MethodPost, host: "bun.lvh.me", path: "/notify", body: []byte(`{"event":"e2e"}`)}.do(t)
	expectStatus(t, small, http.StatusNoContent)
}

func TestCanonicalHostRedirect(t *testing.T) {
	r := get(t, "www.sinatra.lvh.me", "/shop?x=1")
	expectStatus(t, r, http.StatusMovedPermanently)
	if want := "http://sinatra.lvh.me/shop?x=1"; r.Header.Get("Location") != want {
		t.Fatalf("location %q, want %q", r.Header.Get("Location"), want)
	}
}

func TestAppAuthCogLoginStartsTheFlow(t *testing.T) {
	r := get(t, "bun.lvh.me", "/authcog")
	if r.Status != http.StatusFound && r.Status != http.StatusSeeOther {
		t.Fatalf("status %d, want a redirect to AuthCog", r.Status)
	}
	location, err := url.Parse(r.Header.Get("Location"))
	if err != nil || location.Host != "auth.authcog.com" {
		t.Fatalf("location %q, want the auth.authcog.com realm", r.Header.Get("Location"))
	}
	// Only the login path is captured: the rest of the app stays open.
	expectStatus(t, get(t, "bun.lvh.me", "/up"), http.StatusOK)
}

func TestSignInGate(t *testing.T) {
	page := html(t, "button.lvh.me", "/")
	if page.Status != http.StatusFound && page.Status != http.StatusSeeOther {
		t.Fatalf("browser got %d, want a redirect to AuthCog", page.Status)
	}
	if !strings.Contains(page.Header.Get("Location"), "auth.authcog.com") {
		t.Fatalf("location %q", page.Header.Get("Location"))
	}
	// A request that does not want HTML gets 401 instead of a redirect.
	expectStatus(t, get(t, "button.lvh.me", "/"), http.StatusUnauthorized)
	// A forged identity header never reaches the app.
	forged := call{host: "button.lvh.me", path: "/", header: map[string]string{"X-Dboss-User": "root@example.com"}}.do(t)
	expectStatus(t, forged, http.StatusUnauthorized)
}

func TestAppErrorPageFromTemplate(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	page := html(t, "sinatra.lvh.me", "/boom")
	expectStatus(t, page, http.StatusInternalServerError)
	expectBody(t, page, "Sinatra demo")
	// An API client keeps the app's own body.
	plain := get(t, "sinatra.lvh.me", "/boom")
	expectStatus(t, plain, http.StatusInternalServerError)
	if strings.TrimSpace(plain.Body) != "boom" {
		t.Fatalf("plain body %q", plain.Body)
	}
}

func TestMaintenanceMode(t *testing.T) {
	serving(t, "sinatra.lvh.me", "/up")
	api(t, "maintenance", map[string]any{"app": "sinatra", "on": true}, nil)
	defer api(t, "maintenance", map[string]any{"app": "sinatra", "on": false}, nil)
	page := html(t, "sinatra.lvh.me", "/")
	expectStatus(t, page, http.StatusServiceUnavailable)
	expectBody(t, page, "Sinatra demo")
	expectStatus(t, get(t, "sinatra.lvh.me", "/.well-known/dboss/health"), http.StatusServiceUnavailable)
	if s := status(t, "sinatra"); !s.Maintenance || s.State != "running" {
		t.Fatalf("maintenance %v state %s, want maintenance on a running app", s.Maintenance, s.State)
	}
	api(t, "maintenance", map[string]any{"app": "sinatra", "on": false}, nil)
	expectStatus(t, get(t, "sinatra.lvh.me", "/up"), http.StatusOK)
}
