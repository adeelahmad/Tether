package certs

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreatePersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Cert.Equal(b.Cert) {
		t.Fatal("CA regenerated instead of loaded")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, keyFile))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode: %v %v", st.Mode(), err)
		}
	}
}

func TestLeafVerifiesAgainstCA(t *testing.T) {
	a, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)
	for _, host := range []string{"example.com", "mail.example.co.uk:443", "127.0.0.1"} {
		c, err := a.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		name := host
		if h, _, ok := cut(host); ok {
			name = h
		}
		if _, err := c.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
		again, _ := a.Leaf(host)
		if again != c {
			t.Errorf("%s: leaf not cached", host)
		}
	}
	if _, err := a.Leaf(""); err == nil {
		t.Error("empty host accepted")
	}
}

func cut(hp string) (string, string, bool) {
	for i := len(hp) - 1; i >= 0; i-- {
		if hp[i] == ':' {
			return hp[:i], hp[i+1:], true
		}
	}
	return hp, "", false
}

func TestTLSConfigUsesSNIThenFallback(t *testing.T) {
	a, _ := LoadOrCreate(t.TempDir())
	cfg := a.TLSConfig("fallback.example")
	c, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "sni.example"})
	if err != nil || c.Leaf.Subject.CommonName != "sni.example" {
		t.Fatalf("sni: %v", err)
	}
	c, err = cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || c.Leaf.Subject.CommonName != "fallback.example" {
		t.Fatalf("fallback: %v", err)
	}
}

func TestCorruptCA(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, certFile), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(dir, keyFile), []byte("junk"), 0o600)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("corrupt CA accepted")
	}
}
