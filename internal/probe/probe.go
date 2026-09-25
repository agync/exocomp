// Package probe is the shared probe vocabulary: one union Spec that checks
// (internal/reactive) and gates (internal/verify) both build, and one Run that
// decides it. A probe's success is a declared condition — a TCP dial, an ICMP
// answer, an HTTP status, a DNS resolution, a TLS not-after floor, a numeric
// metric threshold — not merely a process exit code.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind names one probe's condition.
type Kind string

// The probe kinds. TCP and ping dial or ICMP an address; the rest are the
// checks verify already declared.
const (
	KindTCP    Kind = "tcp"
	KindPing   Kind = "ping"
	KindHTTP   Kind = "http"
	KindDNS    Kind = "dns"
	KindTLS    Kind = "tls"
	KindMetric Kind = "metric"
)

// DefaultTimeout bounds one probe. A probe that cannot decide in this long is a
// failure, not a hang: an endpoint that never answers is exactly the "exited 0
// but not actually reachable" case a probe exists to catch.
const DefaultTimeout = 10 * time.Second

// Spec is one probe: a kind and the fields that kind reads. Which fields are
// set depends on Kind — tcp uses Address+Port, ping Address, http URL with an
// optional ExpectStatus, dns DNSName with an optional ExpectAddr, tls Address
// (host:port) with MinDays, metric URL with Op and Value.
type Spec struct {
	Kind         Kind
	Address      string
	Port         int
	URL          string
	ExpectStatus int
	DNSName      string
	ExpectAddr   string
	MinDays      int
	Op           string
	Value        float64
	// HTTPAllow3xx widens the default http acceptance (ExpectStatus == 0) from
	// 2xx to 2xx/3xx. The check path sets it; a verify gate does not, so a
	// redirect fails a gate's "the service answered healthily" default while a
	// check calls it up. The two callers disagree deliberately.
	HTTPAllow3xx bool
}

// Result is one probe's outcome. Up is the verdict; Detail explains it either
// way, for the event stream the caller redacts. Duration is how long the run
// took, measured on the wall clock, not on Options.Now.
type Result struct {
	Up       bool
	Duration time.Duration
	Detail   string
}

// Options controls one run. The zero value is usable and uses the process
// defaults; tests inject their own client, resolver, dialer, and clock.
type Options struct {
	// Timeout bounds one probe. Zero uses DefaultTimeout.
	Timeout time.Duration
	// AllowLoopback disables the address-class guard for every kind. The check
	// path leaves it false (a check must not reach the daemon's own loopback,
	// link-local, or unspecified/multicast addresses); a gate sets it true (a
	// gate may probe the host it runs beside).
	AllowLoopback bool
	// HTTPClient is used for http and metric probes. Nil builds a client whose
	// timeout is Timeout (and, when AllowLoopback is false, whose dial is
	// guarded).
	HTTPClient *http.Client
	// Resolver is used for dns probes. Nil uses net.DefaultResolver.
	Resolver *net.Resolver
	// LookupHost overrides host resolution for dns probes. Nil uses Resolver.
	// It is the seam a test resolves through without a network.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// TLSDialer dials a TLS address for tls probes. Nil uses a real handshake
	// with verification disabled (the not-after floor is the property, not the
	// chain).
	TLSDialer func(ctx context.Context, address string) (tls.ConnectionState, error)
	// Now is the clock the tls not-after check reads. Nil uses time.Now.
	Now func() time.Time
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultTimeout
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// validMetricOps are the comparisons a metric probe may declare.
var validMetricOps = map[string]bool{
	"<": true, "<=": true, ">": true, ">=": true, "==": true, "!=": true,
}

// Validate refuses a probe that cannot decide: an unknown kind, or a kind whose
// own fields are incoherent. A probe that cannot decide would silently pass,
// which is the one thing a gate must never do, so this is enforced where the
// probe is built (a check's write, a gate's definition), not at run time where
// a bad probe would already have let a cutover through.
func (s Spec) Validate() error {
	switch s.Kind {
	case KindTCP:
		return s.validateTCP()
	case KindPing:
		return s.validatePing()
	case KindHTTP:
		return s.validateHTTP()
	case KindDNS:
		return s.validateDNS()
	case KindTLS:
		return s.validateTLS()
	case KindMetric:
		return s.validateMetric()
	default:
		return fmt.Errorf("probe: unknown kind %q", s.Kind)
	}
}

func (s Spec) validateTCP() error {
	if _, err := netip.ParseAddr(strings.TrimSpace(s.Address)); err != nil {
		return fmt.Errorf("probe: tcp address %q is not an IP literal: %w", s.Address, err)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("probe: tcp port %d out of range 1..65535", s.Port)
	}
	return nil
}

func (s Spec) validatePing() error {
	if _, err := netip.ParseAddr(strings.TrimSpace(s.Address)); err != nil {
		return fmt.Errorf("probe: ping address %q is not an IP literal: %w", s.Address, err)
	}
	if s.Port != 0 {
		return fmt.Errorf("probe: ping probes an address, not port %d", s.Port)
	}
	return nil
}

func (s Spec) validateHTTP() error {
	if err := requireHTTPURL(s.URL); err != nil {
		return err
	}
	if s.ExpectStatus != 0 && (s.ExpectStatus < 100 || s.ExpectStatus > 599) {
		return fmt.Errorf("probe: http expect_status %d out of range 100..599", s.ExpectStatus)
	}
	return nil
}

func (s Spec) validateDNS() error {
	if strings.TrimSpace(s.DNSName) == "" {
		return fmt.Errorf("probe: dns needs a name to resolve")
	}
	return nil
}

func (s Spec) validateTLS() error {
	addr := strings.TrimSpace(s.Address)
	if addr == "" {
		return fmt.Errorf("probe: tls needs an address (host:port)")
	}
	// Parse rather than merely require a colon: `:443` has a port and no host,
	// and dialing it reaches the local host — the one target a check must not
	// reach.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("probe: tls address %q needs a host:port", s.Address)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("probe: tls address %q needs a host", s.Address)
	}
	p, perr := strconv.Atoi(port)
	if perr != nil || p < 1 || p > 65535 {
		return fmt.Errorf("probe: tls address %q port %q out of range 1..65535", s.Address, port)
	}
	if s.MinDays < 0 {
		return fmt.Errorf("probe: tls min_days cannot be negative")
	}
	return nil
}

func (s Spec) validateMetric() error {
	if err := requireHTTPURL(s.URL); err != nil {
		return err
	}
	if !validMetricOps[s.Op] {
		return fmt.Errorf("probe: metric op %q is not one of < <= > >= == !=", s.Op)
	}
	return nil
}

// requireHTTPURL refuses an empty or non-http(s) URL: a probe whose URL is a
// typo would otherwise fail every run for a reason the operator cannot see.
func requireHTTPURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("probe: needs a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("probe: url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("probe: url %q must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("probe: url %q has no host", raw)
	}
	return nil
}

// Location returns the IP a probe's target resolves to by construction, and
// whether the kind has one. Only an address literal is a location — tcp/ping
// (and a tls Address that is itself a bare IP). A hostname or URL carries no
// location verdict, so dns/tls/metric classify to no netgroup rather than
// assuming an address is an IP literal.
func (s Spec) Location() (netip.Addr, bool) {
	switch s.Kind {
	case KindTCP, KindPing, KindTLS:
		addr, err := netip.ParseAddr(strings.TrimSpace(s.Address))
		if err != nil {
			return netip.Addr{}, false
		}
		return addr, true
	default:
		return netip.Addr{}, false
	}
}
