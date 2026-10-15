package proxy

import (
	"net/http"

	"dboss/internal/config"
	"dboss/internal/httpx"
)

// HostSwitch serves the management console on any of its hostnames and everything else as
// app traffic.
func HostSwitch(managementHosts []string, management, apps http.Handler) http.Handler {
	hosts := make(map[string]bool, len(managementHosts))
	for _, host := range managementHosts {
		hosts[config.NormalizePattern(host)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hosts[config.NormalizeHost(r.Host)] {
			management.ServeHTTP(w, r)
			return
		}
		apps.ServeHTTP(w, r)
	})
}

// CloudflareOnly refuses a connection that does not come from Cloudflare (or the box itself),
// so the origin cannot be reached directly and CF-Connecting-IP cannot be spoofed.
func CloudflareOnly(enabled bool, next http.Handler) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpx.FromCloudflare(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
