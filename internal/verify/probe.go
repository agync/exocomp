// Package verify runs post-run health/verification gates: a probe whose
// success is a declared check — an HTTP status, a DNS resolution, a TLS
// handshake with a not-after floor, or a numeric metric threshold — not merely
// a process exit code. A gate FAILS an otherwise-zero-exit run and blocks the
// rest of a workflow, which is what makes a cutover (the M3c chains, a
// blue/green rollout) safe rather than "the tool exited 0, so it must be up".
//
// Probes run in the daemon over local or otherwise reachable endpoints — a
// control-plane read, like drift collection — and never in a job subprocess:
// there is nothing to sandbox, and the daemon already holds the redactor that
// scrubs a probe's output.
package verify

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// JobKind is the queue/ledger kind a gate step runs under. The daemon routes it
// to the in-process prober rather than to a subprocess driver.
const JobKind = "verify"

// gateArgv0 is the sentinel first argument a gate job carries, so a job row is
// self-describing: the daemon recognises the kind, and the encoded probes ride
// in argv[1] where a resume in a later process can decode them unchanged.
const gateArgv0 = "exocomp-gate"

// DefaultTimeout bounds one probe. A gate that cannot decide in this long is a
// failure, not a hang: an endpoint that never answers is exactly the "exited 0
// but not actually reachable" case the gate exists to catch.
const DefaultTimeout = 10 * time.Second

// ProbeKind names the one check a probe declares.
type ProbeKind string

// The probe kinds.
const (
	KindHTTP   ProbeKind = "http"
	KindDNS    ProbeKind = "dns"
	KindTLS    ProbeKind = "tls"
	KindMetric ProbeKind = "metric"
)

// Probe is one declared health check with a name. Exactly one of the typed
// fields is set; the name is what a failed gate reports, so an operator reading
// "gate failed: web-up" knows which assertion did not hold.
type Probe struct {
	Name   string       `yaml:"name" json:"name"`
	HTTP   *HTTPProbe   `yaml:"http,omitempty" json:"http,omitempty"`
	DNS    *DNSProbe    `yaml:"dns,omitempty" json:"dns,omitempty"`
	TLS    *TLSProbe    `yaml:"tls,omitempty" json:"tls,omitempty"`
	Metric *MetricProbe `yaml:"metric,omitempty" json:"metric,omitempty"`
}

// HTTPProbe passes when a GET of URL returns the expected status. ExpectStatus
// 0 accepts any 2xx, the ordinary "the service answered healthily" check;
// a specific code (e.g. 301, 401) is honoured verbatim when declared.
type HTTPProbe struct {
	URL          string `yaml:"url" json:"url"`
	ExpectStatus int    `yaml:"expect_status,omitempty" json:"expect_status,omitempty"`
}

// DNSProbe passes when Name resolves. When Expect is set, one of the resolved
// addresses must equal it — a DNS cutover is verified by the value it now
// points at, not merely by resolving to something.
type DNSProbe struct {
	Name   string `yaml:"name" json:"name"`
	Expect string `yaml:"expect,omitempty" json:"expect,omitempty"`
}

// TLSProbe passes when a TLS handshake to Address (host:port) completes and the
// leaf certificate's not-after is at least MinDays away. The handshake proves
// the endpoint serves TLS; MinDays is the "warn me before the cert expires"
// ladder every persona asked for. Chain validity is deliberately not required:
// an internal step-ca or self-signed endpoint is a legitimate target, and the
// expiry floor is the property under test.
type TLSProbe struct {
	Address string `yaml:"address" json:"address"`
	MinDays int    `yaml:"min_days,omitempty" json:"min_days,omitempty"`
}

// MetricProbe passes when the numeric body of a GET of URL satisfies Op against
// Value. Op is one of < <= > >= == != . It is the "an exit code is not enough,
// read the number the service reports" check — a readiness gauge, a queue
// depth, a replication lag.
type MetricProbe struct {
	URL   string  `yaml:"url" json:"url"`
	Op    string  `yaml:"op" json:"op"`
	Value float64 `yaml:"value" json:"value"`
}

// validMetricOps are the comparisons a metric probe may declare.
var validMetricOps = map[string]bool{
	"<": true, "<=": true, ">": true, ">=": true, "==": true, "!=": true,
}

// Validate refuses a probe that cannot decide: no name, no kind, more than one
// kind, or a kind whose own fields are incoherent. A gate that cannot decide
// would silently pass, which is the one thing a safety gate must never do — so
// this is enforced at define time (policy add / hooks validate), not at run
// time where a bad gate would already have let a cutover through.
func (p Probe) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("verify: probe has no name")
	}
	set := 0
	if p.HTTP != nil {
		set++
	}
	if p.DNS != nil {
		set++
	}
	if p.TLS != nil {
		set++
	}
	if p.Metric != nil {
		set++
	}
	switch {
	case set == 0:
		return fmt.Errorf("verify: probe %q declares no check (want one of http, dns, tls, metric)", p.Name)
	case set > 1:
		return fmt.Errorf("verify: probe %q declares more than one check; a probe checks exactly one thing", p.Name)
	}

	switch {
	case p.HTTP != nil:
		return p.HTTP.validate(p.Name)
	case p.DNS != nil:
		return p.DNS.validate(p.Name)
	case p.TLS != nil:
		return p.TLS.validate(p.Name)
	default:
		return p.Metric.validate(p.Name)
	}
}

func (h HTTPProbe) validate(name string) error {
	if err := requireHTTPURL(name, h.URL); err != nil {
		return err
	}
	if h.ExpectStatus != 0 && (h.ExpectStatus < 100 || h.ExpectStatus > 599) {
		return fmt.Errorf("verify: probe %q: expect_status %d is not a valid HTTP status", name, h.ExpectStatus)
	}
	return nil
}

func (d DNSProbe) validate(name string) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("verify: probe %q: dns needs a name to resolve", name)
	}
	return nil
}

func (t TLSProbe) validate(name string) error {
	if strings.TrimSpace(t.Address) == "" {
		return fmt.Errorf("verify: probe %q: tls needs an address (host:port)", name)
	}
	if !strings.Contains(t.Address, ":") {
		return fmt.Errorf("verify: probe %q: tls address %q needs a port (host:port)", name, t.Address)
	}
	if t.MinDays < 0 {
		return fmt.Errorf("verify: probe %q: min_days cannot be negative", name)
	}
	return nil
}

func (m MetricProbe) validate(name string) error {
	if err := requireHTTPURL(name, m.URL); err != nil {
		return err
	}
	if !validMetricOps[m.Op] {
		return fmt.Errorf("verify: probe %q: metric op %q is not one of < <= > >= == !=", name, m.Op)
	}
	return nil
}

// requireHTTPURL refuses an empty or non-http(s) URL: a probe whose URL is a
// typo would otherwise fail every run for a reason the operator cannot see.
func requireHTTPURL(name, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("verify: probe %q: needs a url", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("verify: probe %q: url %q: %w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("verify: probe %q: url %q must be http or https", name, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("verify: probe %q: url %q has no host", name, raw)
	}
	return nil
}

// ValidateAll validates a gate's whole probe list and refuses an empty one: a
// gate with no probes is a gate that always passes, which is a gate that is not
// there. A caller that means "no gate" omits the list rather than declaring an
// empty one.
func ValidateAll(probes []Probe) error {
	if len(probes) == 0 {
		return fmt.Errorf("verify: gate declares no probes")
	}
	seen := map[string]bool{}
	for _, p := range probes {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("verify: gate declares two probes named %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// Encode renders a validated probe list into the two-element argv a gate job
// carries: the sentinel, then the base64 of the JSON. It refuses to encode an
// invalid gate, so a job row can never hold a gate that cannot decide.
func Encode(probes []Probe) ([]string, error) {
	if err := ValidateAll(probes); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(probes)
	if err != nil {
		return nil, fmt.Errorf("verify: encode gate: %w", err)
	}
	return []string{gateArgv0, base64.StdEncoding.EncodeToString(raw)}, nil
}

// IsGateArgv reports whether an argv is a gate job's, so a driver can recognise
// one before decoding it.
func IsGateArgv(argv []string) bool {
	return len(argv) >= 1 && argv[0] == gateArgv0
}

// Decode reverses Encode. It re-validates, so a tampered or truncated row is
// refused rather than silently probing nothing.
func Decode(argv []string) ([]Probe, error) {
	if len(argv) != 2 || argv[0] != gateArgv0 {
		return nil, fmt.Errorf("verify: not a gate argv")
	}
	raw, err := base64.StdEncoding.DecodeString(argv[1])
	if err != nil {
		return nil, fmt.Errorf("verify: decode gate: %w", err)
	}
	var probes []Probe
	if err := json.Unmarshal(raw, &probes); err != nil {
		return nil, fmt.Errorf("verify: decode gate: %w", err)
	}
	if err := ValidateAll(probes); err != nil {
		return nil, err
	}
	return probes, nil
}
