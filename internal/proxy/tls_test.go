package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"dboss/internal/config"
)

func TestHostAllowedAllowsConsoleAndRejectsUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.Management.Host = config.List{"dboss.example.com"}
	for _, host := range []string{"dboss.example.com", "DBOSS.example.com:443", "dboss.example.com."} {
		if !hostAllowed(cfg, nil, host) {
			t.Fatalf("console host %q should be allowed", host)
		}
	}
	for _, host := range []string{"", "evil.com", "other.example.com"} {
		if hostAllowed(cfg, nil, host) {
			t.Fatalf("host %q should not be allowed", host)
		}
	}
}

func TestNewCertsInitializesCacheAndALPN(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Proxy.TLS.Listen = ":443"
	certs, err := NewCerts(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "acme")); err != nil {
		t.Fatalf("certificate cache not created: %v", err)
	}
	if !slices.Contains(certs.TLSConfig().NextProtos, acme.ALPNProto) {
		t.Fatalf("ALPN challenge protocol missing from %v", certs.TLSConfig().NextProtos)
	}
}

func TestHTTPHandlerServesForwardedHTTPS(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Proxy.TLS.Listen = ":443"
	certs, err := NewCerts(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := certs.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	forwarded := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	secure := httptest.NewRecorder()
	handler.ServeHTTP(secure, forwarded)
	if secure.Code != http.StatusTeapot {
		t.Fatalf("forwarded https should reach the fallback, got %d", secure.Code)
	}

	plain := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
	redirected := httptest.NewRecorder()
	handler.ServeHTTP(redirected, plain)
	if redirected.Code != http.StatusFound {
		t.Fatalf("plain http should redirect, got %d", redirected.Code)
	}
}

// A name a certificate file covers gets that file; any other name, and an ACME TLS-ALPN-01
// challenge for a covered one, goes to ACME, whose host policy refuses it here.
func TestCertsServesFilesForCoveredHosts(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	certs, err := NewCerts(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.com", "*.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certs.files = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}

	for _, host := range []string{"example.com", "app.example.com", "APP.example.com"} {
		got, err := certs.certificate(&tls.ClientHelloInfo{ServerName: host})
		if err != nil || got != &certs.files[0] {
			t.Errorf("%s: got %v, %v, want the certificate file", host, got, err)
		}
	}
	for _, hello := range []*tls.ClientHelloInfo{
		{ServerName: "a.b.example.com"},
		{ServerName: "other.test"},
		{ServerName: "app.example.com", SupportedProtos: []string{acme.ALPNProto}},
	} {
		if got, err := certs.certificate(hello); err == nil {
			t.Errorf("%s %v: got %v, want ACME to refuse it", hello.ServerName, hello.SupportedProtos, got)
		}
	}
}
