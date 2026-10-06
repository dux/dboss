package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

// /metrics takes the webhook token derived from tokens.dboss, and a hook ping falls back to
// tokens.dboss itself.
func TestWebhookTokenAndTheAdminFallback(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}, token: "s3cret", hookSecrets: map[string]string{"sinatra/deploy": "hook-secret"}}
	handler := newTestHandler(t, manager, nil)
	for _, bearer := range []string{config.Tokens{Dboss: "s3cret"}.WebhookToken(), "s3cret"} {
		request := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("metrics with %q = %d", bearer, response.Code)
		}
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy?token=hook-secret", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy?token=s3cret", strings.NewReader(`{}`)),
		signedHook("s3cret"),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("%s = %d: %s", request.URL, response.Code, response.Body.String())
		}
	}
}

func signedHook(secret string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://dboss.lvh.me:8081/hooks/sinatra/deploy", strings.NewReader(`{}`))
	request.Header.Set("X-Hub-Signature-256", hookSignature(secret, `{}`))
	return request
}

// Past the queue cap every token route refuses at once with Retry-After: /api inside its
// 400 envelope, hooks and /metrics with 429.
func TestTokenRoutesRefuseAQueueingAddress(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}, token: "s3cret", hookSecrets: map[string]string{"sinatra/deploy": "hook-secret"}}
	handler := newTestHandler(t, manager, nil)
	handler.tokens.Spacing = 3 * time.Second
	for range 12 {
		handler.tokens.Reserve("192.0.2.1")
	}
	if code, response := apiPost(t, handler, "/api/ls", "s3cret", `{}`); code != http.StatusBadRequest || response.Error.Code != apiRateLimited {
		t.Fatalf("api = %d %+v", code, response.Error)
	}
	metrics := httptest.NewRequest(http.MethodGet, "http://dboss.lvh.me:8081/metrics", nil)
	metrics.Header.Set("Authorization", "Bearer s3cret")
	for _, request := range []*http.Request{metrics, signedHook("hook-secret")} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
			t.Fatalf("%s = %d %q", request.URL, response.Code, response.Header().Get("Retry-After"))
		}
	}
	if len(manager.actions) != 0 {
		t.Fatalf("a refused call ran %v", manager.actions)
	}
}

// A client that always sends the right token never waits on the spacing.
func TestRightTokensNeverQueue(t *testing.T) {
	manager := &fakeManager{snapshots: []supervisor.Snapshot{{Name: "web", State: supervisor.Running}}, token: "s3cret"}
	handler := newTestHandler(t, manager, nil)
	handler.tokens.Spacing = 3 * time.Second
	started := time.Now()
	for range 5 {
		if code, response := apiPost(t, handler, "/api/ls", "s3cret", `{}`); code != http.StatusOK {
			t.Fatalf("ls = %d %+v", code, response.Error)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("five right calls took %v", elapsed)
	}
}
