package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"":                   "",
		"Bearer abc":         "abc",
		"bearer abc":         "abc",
		"BEARER  abc ":       "abc",
		"Basic dXNlcjpwdw==": "",
		"Bearer":             "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestClientIPTrustsTheHeaderOnlyFromCloudflare(t *testing.T) {
	for remote, want := range map[string]string{
		"173.245.48.9:1234":          "203.0.113.9",
		"[2606:4700::1]:443":         "203.0.113.9",
		"127.0.0.1:5000":             "203.0.113.9",
		"198.51.100.7:1234":          "198.51.100.7",
		"[::ffff:173.245.48.9]:1234": "203.0.113.9",
	} {
		request := httptest.NewRequest("GET", "http://app.example.com/", nil)
		request.RemoteAddr = remote
		request.Header.Set("CF-Connecting-IP", " 203.0.113.9 ")
		request.Header.Set("X-Forwarded-For", "198.51.100.1")
		if got := ClientIP(request); got != want {
			t.Errorf("ClientIP from %s = %q, want %q", remote, got, want)
		}
	}
	direct := httptest.NewRequest("GET", "http://app.example.com/", nil)
	direct.RemoteAddr = "173.245.48.9:1234"
	if got := ClientIP(direct); got != "173.245.48.9" {
		t.Fatalf("no header = %q", got)
	}
}
