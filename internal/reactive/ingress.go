package reactive

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// maxBodyBytes caps one webhook body. An alert group is a few kilobytes of
// labels; a body this large is a bug or an attack.
const maxBodyBytes = 1 << 20

// SignatureHeader carries an optional HMAC of the body under the source's own
// subkey.
const SignatureHeader = "X-Exocomp-Signature"

// Ingress serves the webhook endpoints and the decision links.
//
// Authentication is the path token, which is derived per source and therefore
// unguessable and recoverable from the envelope alone. A body signature is
// verified when the sender sends one — Alertmanager cannot, healthchecks
// cannot, and refusing them would mean refusing the two sources this exists
// for — and a signature that is present and wrong is a refusal.
type Ingress struct {
	engine  *Engine
	deriver Deriver
	allow   []netip.Prefix
	log     *slog.Logger

	// tokens maps a source's derived path token to the source. Lookup is
	// constant-time over the whole set: the token is a bearer capability.
	tokens map[string]string
}

// IngressOptions configures an Ingress.
type IngressOptions struct {
	// Engine decides what each received event is allowed to do.
	Engine *Engine
	// Deriver mints the path tokens and verifies body signatures.
	Deriver Deriver
	// Allow, when non-empty, restricts senders to these CIDRs.
	Allow []netip.Prefix
	// Logger receives ingress failures. Default slog.Default().
	Logger *slog.Logger
}

// NewIngress returns the handler set.
func NewIngress(opts IngressOptions) (*Ingress, error) {
	if opts.Engine == nil {
		return nil, errors.New("reactive: ingress has no engine")
	}
	if opts.Deriver == nil {
		return nil, errors.New("reactive: ingress has no deriver")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	tokens := make(map[string]string, len(Sources))
	for _, source := range Sources {
		token := HookToken(opts.Deriver, source)
		if token == "" {
			return nil, fmt.Errorf("reactive: no hook token for source %q", source)
		}
		tokens[token] = source
	}
	return &Ingress{
		engine:  opts.Engine,
		deriver: opts.Deriver,
		allow:   opts.Allow,
		log:     log,
		tokens:  tokens,
	}, nil
}

// Mounts returns the patterns and handlers to register on the API server's
// mux.
func (in *Ingress) Mounts() map[string]http.Handler {
	return map[string]http.Handler{
		HookPrefix + "{token}":             http.HandlerFunc(in.handleHook),
		ApprovalPrefix + "{id}/{decision}": http.HandlerFunc(in.handleDecision),
	}
}

// ParseAllow turns comma-separated CIDRs into prefixes.
func ParseAllow(spec string) ([]netip.Prefix, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("reactive: %q is not a CIDR: %w", part, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

// peerAddr resolves a request's peer address. It is the same SplitHostPort
// fallback the allowlist uses, factored out so the stamping path and permitted
// read the peer identically: r.RemoteAddr is the address the connection
// arrived on, which over tailnet is the CGNAT 100.64.0.0/10 address.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// permitted reports whether a request's peer is inside the allowlist. An empty
// allowlist permits everything: the path token is the authentication, and a
// CIDR list is defence in depth the operator opts into.
func (in *Ingress) permitted(r *http.Request) bool {
	if len(in.allow) == 0 {
		return true
	}
	addr, ok := peerAddr(r)
	if !ok {
		return false
	}
	for _, prefix := range in.allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// sourceFor resolves a path token to its source without leaking which prefix
// of a wrong token was right.
func (in *Ingress) sourceFor(token string) (string, bool) {
	found := ""
	for candidate, source := range in.tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			found = source
		}
	}
	return found, found != ""
}

// hookResult is what the ingress answers a sender with: one entry per event
// the body described, so a sender's own logs record what the control plane
// decided rather than a bare 202.
type hookResult struct {
	Source string        `json:"source"`
	Events []hookOutcome `json:"events"`
}

type hookOutcome struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	JobID   *int64 `json:"job_id,omitempty"`
}

func (in *Ingress) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !in.permitted(r) {
		in.log.Warn("reactive: webhook from outside the allowlist", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	source, ok := in.sourceFor(r.PathValue("token"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such hook"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	if signature := r.Header.Get(SignatureHeader); signature != "" {
		if err := VerifyBodySignature(in.deriver, source, signature, body); err != nil {
			in.log.Warn("reactive: webhook signature refused", "source", source, "err", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature refused"})
			return
		}
	}

	events, err := Normalize(source, body, in.engine.now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Stamp every event with the network path it arrived on before the pipeline
	// looks at it. The value is the peer's IP and the netgroup containing it
	// (or "away"); see Engine.stampPeer for what that does and does not claim.
	if peer, ok := peerAddr(r); ok {
		for i := range events {
			in.engine.stampPeer(r.Context(), &events[i], peer)
		}
	}

	result := hookResult{Source: source}
	for _, ev := range events {
		stored, err := in.engine.Ingest(r.Context(), ev, body)
		if err != nil {
			in.log.Error("reactive: ingest", "source", source, "event", ev.Name, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ingest failed"})
			return
		}
		result.Events = append(result.Events, hookOutcome{
			ID:      stored.ID,
			Name:    ev.Name,
			Outcome: string(stored.Outcome),
			Reason:  stored.Reason,
			JobID:   stored.JobID,
		})
	}
	writeJSON(w, http.StatusAccepted, result)
}

// handleDecision resolves one decision link. GET is deliberate: this is a
// button on a phone's lock screen, and the capability in the URL is what
// authorizes it. POST is accepted too, for anything that prefers it.
func (in *Ingress) handleDecision(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	id, err := parseApprovalID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such approval"})
		return
	}
	decision := r.PathValue("decision")
	token := r.URL.Query().Get("t")

	result, err := in.engine.Decide(r.Context(), id, decision, token, "link")
	switch {
	case errors.Is(err, ErrStalePlan):
		writeJSON(w, http.StatusConflict, decisionBody(result))
		return
	case errors.Is(err, ErrBadToken), errors.Is(err, store.ErrNotFound):
		// One answer for a wrong token and for an approval that does not
		// exist: which of the two it was is not the caller's business.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this link is not valid"})
		return
	case err != nil:
		in.log.Error("reactive: decide", "approval", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "decision failed"})
		return
	}
	writeJSON(w, http.StatusOK, decisionBody(result))
}

func decisionBody(d Decision) map[string]any {
	return map[string]any{
		"approval": d.Approval.ID,
		"job":      d.Approval.JobID,
		"outcome":  d.Outcome,
		"notice":   d.Message,
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	blob, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(blob)
}
