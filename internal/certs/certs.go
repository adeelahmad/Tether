// Package certs manages the local certificate authority Tether uses to
// terminate HTTPS for proxied requests. A request must be readable to be
// replayed inside the browser, so CONNECT tunnels are intercepted with
// per-host leaf certificates signed by this CA. Clients trust it with
// curl --cacert, or by importing it once.
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	certFile = "ca.pem"
	keyFile  = "ca-key.pem"
)

// Authority is a CA that mints leaf certificates on demand.
type Authority struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     crypto.Signer

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// CertPath returns where the CA certificate lives under dir.
func CertPath(dir string) string { return filepath.Join(dir, certFile) }

// LoadOrCreate loads the CA from dir, creating one (key mode 0600) if none
// exists.
func LoadOrCreate(dir string) (*Authority, error) {
	certPEM, cerr := os.ReadFile(filepath.Join(dir, certFile))
	keyPEM, kerr := os.ReadFile(filepath.Join(dir, keyFile))
	if cerr == nil && kerr == nil {
		return parse(certPEM, keyPEM)
	}
	if !errors.Is(cerr, os.ErrNotExist) && cerr != nil {
		return nil, cerr
	}
	certPEM, keyPEM, err := Generate(time.Now())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, certFile), certPEM, 0o644); err != nil { //nolint:gosec // public certificate
		return nil, err
	}
	return parse(certPEM, keyPEM)
}

// Generate creates a new CA certificate and key, PEM-encoded.
func Generate(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Tether Local CA (" + host + ")", Organization: []string{"Tether"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

func parse(certPEM, keyPEM []byte) (*Authority, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certs: invalid CA: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !cert.IsCA {
		return nil, errors.New("certs: CA key/cert unusable")
	}
	return &Authority{Cert: cert, CertPEM: certPEM, key: signer, cache: map[string]*tls.Certificate{}}, nil
}

// Leaf returns a certificate for host, minting and caching it on first use.
func (a *Authority) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return nil, errors.New("certs: empty host")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[host]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-time.Hour)) {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.Cert.Raw}, PrivateKey: key, Leaf: leaf}
	if len(a.cache) > 4096 {
		a.cache = map[string]*tls.Certificate{}
	}
	a.cache[host] = c
	return c, nil
}

// TLSConfig returns a server config that presents a leaf for the SNI name,
// falling back to fallbackHost (the CONNECT target) when there is none.
func (a *Authority) TLSConfig(fallbackHost string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = fallbackHost
			}
			return a.Leaf(name)
		},
	}
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}
