package executor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// hostKeyEventKey namespaces the host-key-mismatch signal inside an event's
// raw_json.
const hostKeyEventKey = "ansible_hostkey"

// hostKeyChangedMarker is OpenSSH's mismatch-specific banner. It is what
// separates a re-pin (the recorded key no longer matches — a security signal)
// from a first-contact unknown host, which fails with only "Host key
// verification failed" and is not this.
const hostKeyChangedMarker = "REMOTE HOST IDENTIFICATION HAS CHANGED"

// HostKeyMismatch is the parsed security signal: the host whose key changed, the
// key type, the fingerprint it presented, and the known_hosts line the recorded
// pin sits on. It carries no secret — a host key and its fingerprint are public.
type HostKeyMismatch struct {
	Host           string `json:"host"`
	KeyType        string `json:"key_type,omitempty"`
	NewFingerprint string `json:"new_fingerprint,omitempty"`
	KnownHostsRef  string `json:"known_hosts_ref,omitempty"`
}

var (
	reHostKeyHost = regexp.MustCompile(`fatal:\s*\[([^\]]+)\]`)
	reHostKeyType = regexp.MustCompile(`for the (\S+?) key sent by the remote host`)
	// The path stops at whitespace or a backslash, so a single-line JSON result
	// whose message carries literal "\n" escapes does not swallow the next
	// sentence into the known_hosts reference.
	reHostKeyOffend  = regexp.MustCompile(`Offending (\S+?) key in ([^\s\\]+)`)
	reHostKeyNewFing = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]+=*`)
)

// DetectHostKeyMismatch reads an ansible run's output for OpenSSH's changed-key
// banner and pulls out the host and the fingerprint it presented. found is true
// only for the mismatch banner, never for a plain unknown-host failure, so a
// first onboarding is not mistaken for a re-pin. Parsing enriches the signal; a
// run's exit code stays its verdict (AGENTS.md invariant 3).
func DetectHostKeyMismatch(output string) (HostKeyMismatch, bool) {
	if !strings.Contains(output, hostKeyChangedMarker) {
		return HostKeyMismatch{}, false
	}
	var m HostKeyMismatch
	if h := reHostKeyHost.FindStringSubmatch(output); h != nil {
		m.Host = h[1]
	}
	if kt := reHostKeyType.FindStringSubmatch(output); kt != nil {
		m.KeyType = kt[1]
	}
	if off := reHostKeyOffend.FindStringSubmatch(output); off != nil {
		if m.KeyType == "" {
			m.KeyType = off[1]
		}
		m.KnownHostsRef = off[2]
	}
	// The changed-key banner prints exactly one fingerprint: the one the remote
	// host just presented.
	m.NewFingerprint = reHostKeyNewFing.FindString(output)
	return m, true
}

// hostKeyMismatchEvent builds the security event for a detected re-pin. It names
// the host and the presented fingerprint so an operator can compare it against
// the recorded pin before approving a RotateKnownHost.
func hostKeyMismatchEvent(m HostKeyMismatch) Event {
	raw, err := json.Marshal(map[string]HostKeyMismatch{hostKeyEventKey: m})
	if err != nil {
		// Unreachable: the payload is fixed string fields.
		panic("executor: ansible: encode host-key mismatch: " + err.Error())
	}
	msg := fmt.Sprintf("SECURITY: host key changed for %s", m.Host)
	if m.NewFingerprint != "" {
		msg += fmt.Sprintf(" (presented %s)", m.NewFingerprint)
	}
	msg += " — recorded pin no longer matches; host quarantined pending an approved re-pin"
	return Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: msg, RawJSON: string(raw)}
}

// hostKeyRelevant reports whether a line is worth retaining for mismatch
// parsing, so the observer keeps only the handful of banner lines rather than
// the whole run's output.
func hostKeyRelevant(line string) bool {
	switch {
	case strings.Contains(line, hostKeyChangedMarker),
		strings.Contains(line, "Host key verification failed"),
		strings.Contains(line, "Offending"),
		strings.Contains(line, "sent by the remote host"),
		strings.Contains(line, "SHA256:"),
		strings.Contains(line, "UNREACHABLE"),
		strings.Contains(line, "fatal:"):
		return true
	}
	return false
}

// hostKeyObserver forwards every event and retains only the host-key banner
// lines, so any run — check or not — surfaces a re-pin as a security signal
// without changing its streaming or its exit code. A run that never trips the
// mismatch marker retains nothing and behaves exactly as before.
type hostKeyObserver struct {
	inner  EventSink
	mu     sync.Mutex
	lines  []string
	signal bool
}

func (o *hostKeyObserver) Emit(e Event) {
	if hostKeyRelevant(e.Msg) {
		o.mu.Lock()
		o.lines = append(o.lines, e.Msg)
		if strings.Contains(e.Msg, hostKeyChangedMarker) {
			o.signal = true
		}
		o.mu.Unlock()
	}
	o.inner.Emit(e)
}

func (o *hostKeyObserver) mismatch() (HostKeyMismatch, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.signal {
		return HostKeyMismatch{}, false
	}
	return DetectHostKeyMismatch(strings.Join(o.lines, "\n"))
}
