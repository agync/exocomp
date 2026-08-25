package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/pairing"
	"exocomp.dev/exocomp/internal/store"
)

// This file wires the shared pairing contract (internal/pairing) into the
// browser surface: resolving the base URL a phone should dial, auto-naming a
// device, and rendering the pairing QR as inline SVG. The payload — and the
// token it carries — is never logged (invariant 4).

// publicBaseURL is the URL a paired phone should reach this control plane at:
// the operator-configured daemon.public_url when set, else the request's own
// scheme and host so pairing works before an operator configures one. The TLS
// listener means the fallback is always https.
func (s *Server) publicBaseURL(r *http.Request) string {
	if v, err := s.store.GetSetting(r.Context(), store.SettingPublicURL); err == nil {
		if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
			return v
		}
	}
	return "https://" + r.Host
}

// pairingSVG builds the pairing payload for a freshly minted token and renders
// it as inline SVG. The token string is the one-time secret; it lives only in
// the returned markup and the operator's screen, never a log.
func (s *Server) pairingSVG(base, token, scope, name string) (payloadJSON, svg string, err error) {
	p := pairing.New(base, token, s.fingerprint, scope, name)
	if payloadJSON, err = p.JSON(); err != nil {
		return "", "", err
	}
	if svg, err = p.SVG(); err != nil {
		return "", "", err
	}
	return payloadJSON, svg, nil
}

// autoDeviceName mints a friendly, unique device name for a blank-name pair.
// It probes for a free name so the common case hands MintToken a name that
// will not collide; MintToken's own ErrDuplicate retry (handleTokenNew) covers
// the racing-second-operator case.
func (s *Server) autoDeviceName(ctx context.Context) (string, error) {
	for i := 0; i < 8; i++ {
		name := "device-" + randHex(3)
		_, err := s.store.GetTokenByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("web: could not allocate a free device name")
}

// randHex returns n bytes of entropy as lowercase hex.
func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
