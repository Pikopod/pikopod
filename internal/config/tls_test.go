package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTLSConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := "upstreams:\n  widgets:\n    target: https://api.example.invalid\n" + extra
	path := filepath.Join(dir, "pikopod.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTLSHalfConfigRefused(t *testing.T) {
	path := writeTLSConfig(t, "tls:\n  cert_file: /tmp/cert.pem\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "both cert_file and key_file") {
		t.Fatalf("half a TLS config must refuse: %v", err)
	}
}

func TestTLSMissingFilesRefused(t *testing.T) {
	path := writeTLSConfig(t, "tls:\n  cert_file: /nonexistent/cert.pem\n  key_file: /nonexistent/key.pem\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("missing TLS files must refuse: %v", err)
	}
}

// selfSigned writes a fresh self-signed localhost cert+key pair.
func selfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pikopod"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certPath)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyPath)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyOut.Close()
	return certPath, keyPath
}

func TestSchemeAndLocalClientTrustConfiguredCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := selfSigned(t, dir)
	cfgPath := writeTLSConfig(t, "tls:\n  cert_file: "+certPath+"\n  key_file: "+keyPath+"\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheme() != "https" {
		t.Fatalf("scheme: %s", cfg.Scheme())
	}

	// Serve with the configured pair; the LocalClient must trust it while a
	// default client (system roots) must refuse it.
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()

	resp, err := cfg.LocalClient(2 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("LocalClient must trust the configured cert: %v", err)
	}
	resp.Body.Close()
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Get(srv.URL); err == nil {
		t.Fatal("a default client must NOT trust the self-signed cert")
	}
}

func TestSchemeDefaultsToHTTP(t *testing.T) {
	cfg, err := Load(writeTLSConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheme() != "http" {
		t.Fatalf("scheme: %s", cfg.Scheme())
	}
}
