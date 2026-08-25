// Package pairing is the phone-companion device-pairing contract: the JSON a
// QR code carries and the two renderers that draw it — an inline SVG for the
// browser surface and a half-block QR for a terminal. It is deliberately
// web-agnostic so both internal/web and the CLI import the one payload shape.
package pairing

import (
	"encoding/json"
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Version is the payload schema version carried in the "v" field. A scanner
// keys its parsing off this, so it only ever increments.
const Version = 1

// Payload is the phone-facing pairing contract encoded into a QR code. It
// carries everything a companion needs to reach and authenticate against this
// control plane: the base URL, a freshly minted access token, the certificate
// fingerprint to trust-on-first-use pin, the token's scope, and the device
// name. It never carries the raw argon2id-hashed secret separately — the token
// string already is the one-time credential.
type Payload struct {
	V           int    `json:"v"`
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Scope       string `json:"scope"`
	Name        string `json:"name"`
}

// New builds a v1 payload.
func New(baseURL, token, fingerprint, scope, name string) Payload {
	return Payload{
		V: Version, URL: baseURL, Token: token,
		Fingerprint: fingerprint, Scope: scope, Name: name,
	}
}

// JSON marshals the payload to its canonical wire form — the exact bytes the
// QR encodes. Callers must never log the result; it holds the token.
func (p Payload) JSON() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// qr encodes the payload JSON at medium error-correction, the level that
// survives a phone camera's usual glare without bloating the module count.
func (p Payload) qr() (*qrcode.QRCode, error) {
	js, err := p.JSON()
	if err != nil {
		return nil, err
	}
	return qrcode.New(js, qrcode.Medium)
}

// svgMargin is the quiet zone, in modules, a scanner needs around the code.
const svgMargin = 4

// SVG renders the payload as a self-contained inline <svg>: one <rect> per dark
// module over a white backing, so it scans in either page theme. The module
// fill is currentColor and the wrapper the caller places it in forces a dark
// ink on the white backing (see .pairqr). The viewBox is in module units, so
// the width/height alone size it. No external assets, no script.
func (p Payload) SVG() (string, error) {
	qr, err := p.qr()
	if err != nil {
		return "", err
	}
	qr.DisableBorder = true // the quiet zone is drawn here, in module units
	bm := qr.Bitmap()
	n := len(bm)
	dim := n + 2*svgMargin

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="180" height="180" `+
		`viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" `+
		`aria-label="Device pairing QR code">`, dim, dim)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, dim, dim)
	b.WriteString(`<g fill="currentColor">`)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if bm[y][x] {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, x+svgMargin, y+svgMargin)
			}
		}
	}
	b.WriteString(`</g></svg>`)
	return b.String(), nil
}

// Terminal renders the payload as a scannable half-block QR for a TTY, each
// terminal row packing two module rows. A trailing newline keeps it clear of
// the next line of output.
func (p Payload) Terminal() (string, error) {
	qr, err := p.qr()
	if err != nil {
		return "", err
	}
	return qr.ToSmallString(false), nil
}
