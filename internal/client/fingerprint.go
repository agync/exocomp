package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"strings"
)

// Fingerprint renders a DER certificate's SHA-256 as the colon-grouped
// uppercase hex the daemon prints (internal/web/tls.go fingerprint) and shows on
// its login screen. The client and the daemon therefore agree on the exact
// string an operator compares by eye, so a fingerprint pinned by `exocomp login`
// is the same one `token`/`doctor` print on the box.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	out := make([]byte, 0, len(sum)*3)
	const hexdigits = "0123456789ABCDEF"
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out)
}

// sameFingerprint compares two fingerprints case- and separator-insensitively,
// so a pin an operator pasted lowercase, or with no colons, still matches. The
// comparison is on content, not formatting.
func sameFingerprint(a, b string) bool {
	return normalizeFingerprint(a) == normalizeFingerprint(b)
}

func normalizeFingerprint(s string) string {
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToUpper(strings.TrimSpace(s))
}

// FetchFingerprint dials the endpoint's TLS listener and returns the leaf
// certificate's fingerprint WITHOUT pinning — the trust-on-first-use capture
// `exocomp login` shows the operator before storing. Verification is disabled on
// purpose: there is no pin yet, and the whole point is to learn the cert so a
// later connection can pin it. It sends no token and reads no HTTP; it is a bare
// handshake.
func FetchFingerprint(ctx context.Context, endpoint string) (string, error) {
	addr, err := hostPort(endpoint)
	if err != nil {
		return "", err
	}
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // TOFU capture: no pin exists yet, learning the cert is the point
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("client: connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("client: %s presented no certificate", addr)
	}
	return Fingerprint(state.PeerCertificates[0].Raw), nil
}
