package metrics

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// ScrapePurpose is the HKDF purpose the metrics scrape bearer is derived under.
// Like the state backend's per-workspace token (ADR 0004), it is a subkey of
// the envelope DEK, so it is deterministic — the daemon, `exocomp doctor`, and
// a recovery-time operator with the envelope all compute the same string — and
// nothing extra is stored at rest.
const ScrapePurpose = "metrics-scrape"

// Deriver is the slice of the secrets envelope this package needs: one HKDF
// subkey for the scrape bearer. Nothing here holds a secret of its own.
type Deriver interface {
	Derive(purpose, id string) []byte
}

// ScrapeToken returns the bearer that authenticates a metrics scrape. Configure
// Prometheus with it (an `authorization` credential), or read it from `exocomp
// doctor`. It is base64 of the 32-byte subkey — the same shape the state
// backend's token takes.
func ScrapeToken(d Deriver) string {
	return base64.StdEncoding.EncodeToString(d.Derive(ScrapePurpose, ""))
}

// Authenticated wraps a metrics handler so every scrape must present the scrape
// bearer. The endpoint shares the loopback state-backend listener, which
// tofu-tier jobs and anything fronting the webhook path can already reach
// (finding D3), so it cannot be left open the way a dedicated bind might: a
// job could otherwise read the operator's whole staleness picture. The compare
// is constant-time over the derived token; a nil deriver is a programming error
// and fails every scrape closed rather than silently serving in the clear.
func Authenticated(next http.Handler, d Deriver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d == nil || !scrapeAuthorized(r, d) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="exocomp-metrics"`)
			http.Error(w, "# unauthorized: present the metrics scrape bearer\n", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func scrapeAuthorized(r *http.Request, d Deriver) bool {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	want := ScrapeToken(d)
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}
