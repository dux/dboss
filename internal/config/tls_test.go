package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePair writes a self-signed certificate for hosts, valid until notAfter, and its key.
func writePair(t *testing.T, dir, name string, notAfter time.Time, hosts ...string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: hosts, NotBefore: notAfter.Add(-48 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseProxyTLSCerts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(24 * time.Hour)
	writePair(t, filepath.Join(dir, "certs"), "good", later, "example.com", "*.example.com")
	writePair(t, filepath.Join(dir, "certs"), "other", later, "example.com")
	writePair(t, filepath.Join(dir, "certs"), "expired", time.Now().Add(-time.Hour), "example.com")
	path := filepath.Join(dir, ServerFileName)
	host := func(tls string) string { return "apps: ./apps\nproxy:\n  tls:\n" + tls }

	cfg, err := Parse([]byte(host("    listen: \":443\"\n    certs: [{cert: certs/good.pem, key: certs/good.key}]\n")), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Proxy.TLS.Certs[0]; got.Cert != filepath.Join(dir, "certs", "good.pem") || got.Key != filepath.Join(dir, "certs", "good.key") {
		t.Fatalf("paths not resolved against the config dir: %+v", got)
	}

	for name, tc := range map[string]struct{ tls, want string }{
		"no listen":  {"    certs: [{cert: certs/good.pem, key: certs/good.key}]\n", "needs listen"},
		"no key":     {"    listen: \":443\"\n    certs: [{cert: certs/good.pem}]\n", "needs cert and key"},
		"missing":    {"    listen: \":443\"\n    certs: [{cert: certs/none.pem, key: certs/none.key}]\n", "no such file"},
		"mismatched": {"    listen: \":443\"\n    certs: [{cert: certs/good.pem, key: certs/other.key}]\n", "does not match"},
		"expired":    {"    listen: \":443\"\n    certs: [{cert: certs/expired.pem, key: certs/expired.key}]\n", "expired on"},
	} {
		_, err := Parse([]byte(host(tc.tls)), path)
		if err == nil || !strings.Contains(err.Error(), "proxy.tls.certs") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want a proxy.tls.certs error with %q", name, err, tc.want)
		}
	}
}
