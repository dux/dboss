package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

const rateLimitedConfig = "rate_limit:\n  requests: 2\n  window: 60s\n  paths: [\"/api/*\"]\n  methods: [POST]\n"

// rateHandler is the feature handler with a limiter on a fixed clock and only the rate-limit
// stage in front of an always-200 capture.
func rateHandler(t *testing.T, data string, now *int64) (*Handler, supervisor.Snapshot) {
	t.Helper()
	handler := featureHandler()
	handler.limiter = newRateLimiter()
	handler.limiter.now = func() int64 { return *now }
	snapshot := featureSnapshot(t, data)
	handler.filters = []Filter{handler.rateLimit, func(w http.ResponseWriter, _ *http.Request, _ supervisor.Snapshot, _ func()) {
		w.WriteHeader(http.StatusOK)
	}}
	return handler, snapshot
}

func rateRequest(method, target, ip string, html bool) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = ip + ":12345"
	if html {
		request.Header.Set("Accept", "text/html")
	}
	return request
}

func TestRateLimitCountsOnlyMatchingRequests(t *testing.T) {
	now := int64(1000)
	handler, snapshot := rateHandler(t, rateLimitedConfig, &now)
	for i := 0; i < 2; i++ {
		if r := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/login", "203.0.113.5", false)); r.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, r.Code)
		}
	}
	limited := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/login", "203.0.113.5", false))
	if limited.Code != http.StatusTooManyRequests || limited.Body.Len() != 0 || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("third = %d %q retry=%q", limited.Code, limited.Body.String(), limited.Header().Get("Retry-After"))
	}
	// A request matching only one dimension is never charged, so it stays within the limit.
	for _, request := range []*http.Request{
		rateRequest(http.MethodGet, "http://demo.test/api/login", "203.0.113.5", false),
		rateRequest(http.MethodPost, "http://demo.test/other", "203.0.113.5", false),
	} {
		if r := serveFeature(t, handler, snapshot, request); r.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", request.Method, request.URL.Path, r.Code)
		}
	}
}

func TestRateLimitPageForBrowsers(t *testing.T) {
	now := int64(1000)
	handler, snapshot := rateHandler(t, "rate_limit:\n  requests: 1\n  window: 60s\n", &now)
	serveFeature(t, handler, snapshot, rateRequest(http.MethodGet, "http://demo.test/page", "203.0.113.5", false))
	response := serveFeature(t, handler, snapshot, rateRequest(http.MethodGet, "http://demo.test/page", "203.0.113.5", true))
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "Too many requests") {
		t.Fatalf("html 429 = %d %q", response.Code, response.Body.String())
	}
}

func TestRateLimitRetryAfterTracksTheWindow(t *testing.T) {
	now := int64(1000)
	handler, snapshot := rateHandler(t, rateLimitedConfig, &now)
	serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false))
	serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false))

	now = 1030
	limited := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false))
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "30" {
		t.Fatalf("mid-window = %d retry=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	// A full window after the first request ages out, the client is admitted again.
	now = 1060
	if r := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false)); r.Code != http.StatusOK {
		t.Fatalf("after the window = %d", r.Code)
	}
}

func TestRateLimitIsPerIP(t *testing.T) {
	now := int64(1000)
	handler, snapshot := rateHandler(t, rateLimitedConfig, &now)
	for i := 0; i < 2; i++ {
		serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false))
	}
	if r := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false)); r.Code != http.StatusTooManyRequests {
		t.Fatalf("first IP should be limited: %d", r.Code)
	}
	if r := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "198.51.100.9", false)); r.Code != http.StatusOK {
		t.Fatalf("a different IP should pass: %d", r.Code)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	now := int64(1000)
	handler, snapshot := rateHandler(t, "rate_limit:\n  requests: 0\n", &now)
	for i := 0; i < 5; i++ {
		if r := serveFeature(t, handler, snapshot, rateRequest(http.MethodPost, "http://demo.test/api/x", "203.0.113.5", false)); r.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, r.Code)
		}
	}
	if len(handler.limiter.clients) != 0 {
		t.Fatalf("a disabled limit must keep no counters: %d", len(handler.limiter.clients))
	}
}

func TestRateLimiterSweepDropsIdleClients(t *testing.T) {
	now := int64(1000)
	limiter := newRateLimiter()
	limiter.now = func() int64 { return now }
	rl := config.RateLimit{Requests: 5, Window: config.Duration(60 * time.Second)}
	limiter.allow("demo", "1.1.1.1", rl)
	limiter.allow("demo", "2.2.2.2", rl)
	limiter.sweep(1000)
	if len(limiter.clients) != 2 {
		t.Fatalf("live clients = %d, want 2", len(limiter.clients))
	}
	limiter.sweep(1061)
	if len(limiter.clients) != 0 {
		t.Fatalf("idle clients = %d, want 0", len(limiter.clients))
	}
}

func TestRateLimiterTrimEvictsOldestPastCap(t *testing.T) {
	now := int64(1000)
	limiter := newRateLimiter()
	limiter.now = func() int64 { return now }
	rl := config.RateLimit{Requests: 5, Window: config.Duration(600 * time.Second)}
	limiter.allow("demo", "1.1.1.1", rl)
	now = 1005
	limiter.allow("demo", "2.2.2.2", rl)
	now = 1010
	limiter.allow("demo", "3.3.3.3", rl)
	limiter.trim(1010, 2)
	if len(limiter.clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(limiter.clients))
	}
	if _, ok := limiter.clients[rateKey{app: "demo", ip: "1.1.1.1"}]; ok {
		t.Fatal("the least recently seen client should be evicted")
	}
}

func TestRateLimiterResetsWhenWindowChanges(t *testing.T) {
	now := int64(1000)
	limiter := newRateLimiter()
	limiter.now = func() int64 { return now }
	short := config.RateLimit{Requests: 2, Window: config.Duration(30 * time.Second)}
	long := config.RateLimit{Requests: 2, Window: config.Duration(60 * time.Second)}
	limiter.allow("demo", "1.1.1.1", short)
	limiter.allow("demo", "1.1.1.1", short)
	if ok, _ := limiter.allow("demo", "1.1.1.1", short); ok {
		t.Fatal("the short window should be full")
	}
	// A rescan that changes the window size starts that client's counters fresh.
	if ok, _ := limiter.allow("demo", "1.1.1.1", long); !ok {
		t.Fatal("a changed window should reset the counters")
	}
}

func TestHandlerStartAndClose(t *testing.T) {
	// A Handler never started must Close cleanly.
	if err := (&Handler{limiter: newRateLimiter()}).Close(); err != nil {
		t.Fatalf("close before start: %v", err)
	}
	handler := &Handler{limiter: newRateLimiter()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.Start(ctx)
	if err := handler.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := handler.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
