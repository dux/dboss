// Package httpx holds request helpers shared by the console, the proxy and pubsub.
package httpx

import (
	"net"
	"net/http"
	"net/netip"
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

// ClientIP is the visitor's address: CF-Connecting-IP when the connection comes from Cloudflare,
// which is the only party that can set it there, else the connection's own address. A host that
// is partly reached around Cloudflare (a DNS-only name) still sees real visitors on both paths.
func ClientIP(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); value != "" && FromCloudflare(r.RemoteAddr) {
		return value
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// FromCloudflare reports whether a connection address, with or without a port, is one of
// Cloudflare's published ranges or the box itself.
func FromCloudflare(address string) bool {
	return InPrefixes(address, cloudflareRanges)
}

// InPrefixes reports whether an address, with or without a port, lies in any of prefixes.
func InPrefixes(address string, prefixes []netip.Prefix) bool {
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// cloudflareRanges are the addresses Cloudflare connects from, as published at
// https://www.cloudflare.com/ips/.
var cloudflareRanges = []netip.Prefix{
	netip.MustParsePrefix("173.245.48.0/20"),
	netip.MustParsePrefix("103.21.244.0/22"),
	netip.MustParsePrefix("103.22.200.0/22"),
	netip.MustParsePrefix("103.31.4.0/22"),
	netip.MustParsePrefix("141.101.64.0/18"),
	netip.MustParsePrefix("108.162.192.0/18"),
	netip.MustParsePrefix("190.93.240.0/20"),
	netip.MustParsePrefix("188.114.96.0/20"),
	netip.MustParsePrefix("197.234.240.0/22"),
	netip.MustParsePrefix("198.41.128.0/17"),
	netip.MustParsePrefix("162.158.0.0/15"),
	netip.MustParsePrefix("104.16.0.0/13"),
	netip.MustParsePrefix("104.24.0.0/14"),
	netip.MustParsePrefix("172.64.0.0/13"),
	netip.MustParsePrefix("131.0.72.0/22"),
	netip.MustParsePrefix("2400:cb00::/32"),
	netip.MustParsePrefix("2606:4700::/32"),
	netip.MustParsePrefix("2803:f800::/32"),
	netip.MustParsePrefix("2405:b500::/32"),
	netip.MustParsePrefix("2405:8100::/32"),
	netip.MustParsePrefix("2a06:98c0::/29"),
	netip.MustParsePrefix("2c0f:f248::/32"),
	// Loopback, so a check or a tunnel on the box itself still reaches the proxy.
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}
