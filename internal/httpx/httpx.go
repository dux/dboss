// Package httpx holds request helpers shared by the console, the proxy and pubsub.
package httpx

import (
	"net"
	"net/http"
	"strings"
)

// BearerToken is the credential of an "Authorization: Bearer <token>" header, or "" without one.
// The scheme is case-insensitive, as RFC 7235 says.
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// ClientIP is the visitor's address: CF-Connecting-IP behind Cloudflare, where only Cloudflare
// can connect and the header cannot be spoofed, else the connection's own address.
func ClientIP(r *http.Request, cloudflare bool) string {
	if value := r.Header.Get("CF-Connecting-IP"); cloudflare && value != "" {
		return strings.TrimSpace(value)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
