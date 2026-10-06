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

func TestClientIPTrustsTheHeaderOnlyBehindCloudflare(t *testing.T) {
	request := httptest.NewRequest("GET", "http://app.example.com/", nil)
	request.RemoteAddr = "173.245.48.9:1234"
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := ClientIP(request, true); got != "203.0.113.9" {
		t.Fatalf("behind cloudflare = %q", got)
	}
	if got := ClientIP(request, false); got != "173.245.48.9" {
		t.Fatalf("direct = %q", got)
	}
}
