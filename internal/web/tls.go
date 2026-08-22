package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certFile and keyFile are where the LAN listener's self-signed material lives
// in the data dir. The key is written 0600 and never leaves the box; the cert
// is what a phone pins on first use (ADR 0009).
const (
	certFile = "web-cert.pem"
	keyFile  = "web-key.pem"
)

// certValidity is how long a generated certificate is good for. It is long
// because trust-on-first-use pins the fingerprint, not a CA chain: a rotation
// is a re-pin the operator chooses, not a clock the daemon forces.
const certValidity = 10 * 365 * 24 * time.Hour

// EnsureCert loads the LAN certificate from the data dir, generating a
// self-signed one on first call. It returns the certificate and its SHA-256
// fingerprint, the string an operator pins and `token` and `doctor` print.
func EnsureCert(dataDir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dataDir, certFile)
	keyPath := filepath.Join(dataDir, keyFile)

	if _, err := os.Stat(certPath); err == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("web: load certificate: %w", err)
		}
		fp, err := fingerprint(cert)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		return cert, fp, nil
	}

	cert, err := generateCert(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	fp, err := fingerprint(cert)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, fp, nil
}

// Fingerprint is the SHA-256 of a certificate, printed for pinning. It is
// exported so the CLI and doctor render the exact string a browser shows.
func Fingerprint(dataDir string) (string, error) {
	_, fp, err := EnsureCert(dataDir)
	return fp, err
}

func generateCert(certPath, keyPath string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web: serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "exocomp"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// The listener is reached by whatever LAN name or address the operator
		// points at it; TOFU pins the fingerprint, so the SANs only have to
		// cover the box itself talking to its own loopback.
		DNSNames:    []string{"exocomp", "localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// The key is 0600 and lands before the cert, so a crash never leaves a cert
	// whose key is missing or world-readable.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("web: write key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("web: write certificate: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web: keypair: %w", err)
	}
	return cert, nil
}

// fingerprint renders the SHA-256 of the leaf certificate as the colon-grouped
// uppercase hex a browser's certificate dialog shows.
func fingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("web: certificate has no leaf")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	out := make([]byte, 0, len(sum)*3)
	const hexdigits = "0123456789ABCDEF"
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out), nil
}
