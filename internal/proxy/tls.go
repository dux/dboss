package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"dboss/internal/config"
	"dboss/internal/supervisor"
)

// Certs terminates TLS. A hostname covered by one of the configured certificate files gets that
// certificate; any other hostname that resolves to one of the running apps or to the console gets
// an ACME certificate the first time it is requested; no other name does, so a stranger cannot
// make dboss exhaust the certificate authority's rate limit.
type Certs struct {
	manager *autocert.Manager
	files   []tls.Certificate
}

// NewCerts loads the certificate files and builds the certificate manager against Let's Encrypt
// production, caching under <state_dir>/acme. AcceptTOS is granted because the operator enables
// TLS.
func NewCerts(cfg config.Config, manager *supervisor.Manager) (*Certs, error) {
	files := make([]tls.Certificate, 0, len(cfg.Proxy.TLS.Certs))
	for _, file := range cfg.Proxy.TLS.Certs {
		pair, err := file.Load()
		if err != nil {
			return nil, fmt.Errorf("proxy.tls.certs: %w", err)
		}
		files = append(files, pair)
	}
	cache := filepath.Join(cfg.StateDir, "acme")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("acme cache: %w", err)
	}
	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(cache),
		Email:  cfg.Proxy.TLS.Email,
		Client: &acme.Client{},
		HostPolicy: func(_ context.Context, host string) error {
			if hostAllowed(cfg, manager, host) {
				return nil
			}
			return fmt.Errorf("dboss: no app or console is configured for %q", host)
		},
	}
	return &Certs{manager: m, files: files}, nil
}

// TLSConfig is the server TLS configuration: on-demand certificate lookup plus the ALPN
// protocol the ACME TLS-ALPN-01 challenge uses.
func (c *Certs) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: c.certificate,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
	}
}

// Hosts lists the hostnames the certificate files cover, for the startup log.
func (c *Certs) Hosts() []string {
	var hosts []string
	for _, file := range c.files {
		hosts = append(hosts, file.Leaf.DNSNames...)
	}
	return hosts
}

// certificate picks the first certificate file that covers the requested name and leaves the
// rest, and every TLS-ALPN-01 challenge, to ACME.
func (c *Certs) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello.ServerName != "" && !slices.Contains(hello.SupportedProtos, acme.ALPNProto) {
		for i := range c.files {
			if c.files[i].Leaf.VerifyHostname(hello.ServerName) == nil {
				return &c.files[i], nil
			}
		}
	}
	return c.manager.GetCertificate(hello)
}

// HTTPHandler answers ACME HTTP-01 challenges on the plain-HTTP listener. A request whose public
// scheme is already https - Cloudflare sets X-Forwarded-Proto when it terminates TLS and reaches
// the origin over http (Flexible) - is served by the fallback, so a Flexible host keeps working
// while dboss also terminates TLS for the hosts that reach it over https. Every other request
// redirects to https.
func (c *Certs) HTTPHandler(fallback http.Handler) http.Handler {
	served := c.manager.HTTPHandler(fallback)
	redirect := c.manager.HTTPHandler(nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestScheme(r) == "https" {
			served.ServeHTTP(w, r)
			return
		}
		redirect.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether host belongs to an app of this host or to the console. It is the
// on-demand policy, evaluated against the live app table so a rescanned app needs no restart.
func hostAllowed(cfg config.Config, manager *supervisor.Manager, host string) bool {
	host = config.NormalizeHost(host)
	if host == "" {
		return false
	}
	for _, name := range cfg.Management.Host {
		if config.NormalizePattern(name) == host {
			return true
		}
	}
	if manager == nil {
		return false
	}
	_, ok := manager.ResolveHost(host)
	return ok
}
