package verify

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxMetricBody caps how much of a metric endpoint's body is read before it is
// parsed as a number. A readiness gauge is a handful of bytes; anything past
// this is not the number the probe was pointed at.
const maxMetricBody = 4 << 10

// Result is one probe's outcome. OK is the only thing that decides the gate;
// Detail explains it either way, for the event stream the daemon redacts.
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Runner performs probes. The zero value is usable and uses the process
// defaults; tests inject their own client, resolver, and clock. A Runner holds
// no secret material — the redactor that scrubs its output lives in the daemon
// sink, not here.
type Runner struct {
	// HTTPClient is used for http and metric probes. Nil uses a client whose
	// timeout is the per-probe timeout.
	HTTPClient *http.Client
	// Resolver is used for dns probes. Nil uses net.DefaultResolver.
	Resolver *net.Resolver
	// LookupHost overrides host resolution for dns probes. Nil uses Resolver.
	// It is the seam a test resolves through without a network.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// TLSDialer dials a TLS address for tls probes. Nil uses a real handshake
	// with verification disabled (the not-after floor is the property, not the
	// chain; see TLSProbe).
	TLSDialer func(ctx context.Context, address string) (tls.ConnectionState, error)
	// Now is the clock the tls not-after check reads. Nil uses time.Now.
	Now func() time.Time
	// Timeout bounds one probe. Zero uses DefaultTimeout.
	Timeout time.Duration
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// Run performs one probe and returns its result. An error reaching the endpoint
// is a failed probe, not a Run error: a gate whose target is unreachable has
// failed, and returning that as OK=false is exactly the point.
func (r Runner) Run(ctx context.Context, p Probe) Result {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	switch {
	case p.HTTP != nil:
		return r.runHTTP(ctx, p.Name, *p.HTTP)
	case p.DNS != nil:
		return r.runDNS(ctx, p.Name, *p.DNS)
	case p.TLS != nil:
		return r.runTLS(ctx, p.Name, *p.TLS)
	case p.Metric != nil:
		return r.runMetric(ctx, p.Name, *p.Metric)
	default:
		return Result{Name: p.Name, OK: false, Detail: "probe declares no check"}
	}
}

// Sink receives one line per probe as a gate runs, so the failing probe is
// visible in the job's event stream and not only in the returned verdict.
type Sink func(line string)

// RunAll runs every probe and reports whether the gate passed. It runs all of
// them even after one fails, so the operator sees the whole picture in one run
// rather than fixing them one at a time. The gate passes only if every probe
// passed; failed names the probes that did not.
func (r Runner) RunAll(ctx context.Context, probes []Probe, emit Sink) (ok bool, failed []string) {
	ok = true
	for _, p := range probes {
		res := r.Run(ctx, p)
		verdict := "pass"
		if !res.OK {
			verdict = "FAIL"
			ok = false
			failed = append(failed, res.Name)
		}
		if emit != nil {
			emit(fmt.Sprintf("probe %s: %s (%s)", res.Name, verdict, res.Detail))
		}
	}
	return ok, failed
}

func (r Runner) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: r.timeout()}
}

func (r Runner) runHTTP(ctx context.Context, name string, h HTTPProbe) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return fail(name, "build request: %v", err)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fail(name, "GET %s: %v", h.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetricBody))

	if h.ExpectStatus == 0 {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return pass(name, "GET %s -> %d", h.URL, resp.StatusCode)
		}
		return fail(name, "GET %s -> %d, want 2xx", h.URL, resp.StatusCode)
	}
	if resp.StatusCode == h.ExpectStatus {
		return pass(name, "GET %s -> %d", h.URL, resp.StatusCode)
	}
	return fail(name, "GET %s -> %d, want %d", h.URL, resp.StatusCode, h.ExpectStatus)
}

func (r Runner) runDNS(ctx context.Context, name string, d DNSProbe) Result {
	lookup := r.LookupHost
	if lookup == nil {
		res := r.Resolver
		if res == nil {
			res = net.DefaultResolver
		}
		lookup = res.LookupHost
	}
	addrs, err := lookup(ctx, d.Name)
	if err != nil {
		return fail(name, "resolve %s: %v", d.Name, err)
	}
	if len(addrs) == 0 {
		return fail(name, "resolve %s: no addresses", d.Name)
	}
	if d.Expect == "" {
		return pass(name, "%s -> %s", d.Name, strings.Join(addrs, ","))
	}
	if slices.Contains(addrs, d.Expect) {
		return pass(name, "%s -> %s (has %s)", d.Name, strings.Join(addrs, ","), d.Expect)
	}
	return fail(name, "%s -> %s, want %s", d.Name, strings.Join(addrs, ","), d.Expect)
}

func (r Runner) runTLS(ctx context.Context, name string, t TLSProbe) Result {
	dial := r.TLSDialer
	if dial == nil {
		dial = defaultTLSDial
	}
	state, err := dial(ctx, t.Address)
	if err != nil {
		return fail(name, "handshake %s: %v", t.Address, err)
	}
	if len(state.PeerCertificates) == 0 {
		return fail(name, "handshake %s: peer sent no certificate", t.Address)
	}
	leaf := state.PeerCertificates[0]
	days := int(leaf.NotAfter.Sub(r.now()).Hours() / 24)
	if t.MinDays > 0 && days < t.MinDays {
		return fail(name, "%s cert expires in %dd (not-after %s), want >= %dd",
			t.Address, days, leaf.NotAfter.UTC().Format(time.RFC3339), t.MinDays)
	}
	return pass(name, "%s handshake ok, cert valid %dd (not-after %s)",
		t.Address, days, leaf.NotAfter.UTC().Format(time.RFC3339))
}

// defaultTLSDial performs a real handshake and returns the connection state.
// Verification is disabled on purpose: the not-after floor is what the tls
// probe asserts, and an internal or self-signed endpoint is a valid target.
func defaultTLSDial(ctx context.Context, address string) (tls.ConnectionState, error) {
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // not-after floor is the check, not chain validity
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState(), nil
}

func (r Runner) runMetric(ctx context.Context, name string, m MetricProbe) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return fail(name, "build request: %v", err)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fail(name, "GET %s: %v", m.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricBody))
	if err != nil {
		return fail(name, "read %s: %v", m.URL, err)
	}
	got, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil {
		return fail(name, "%s body %q is not a number", m.URL, strings.TrimSpace(string(body)))
	}
	if metricHolds(got, m.Op, m.Value) {
		return pass(name, "%s = %v %s %v", m.URL, got, m.Op, m.Value)
	}
	return fail(name, "%s = %v, want %s %v", m.URL, got, m.Op, m.Value)
}

// metricHolds evaluates got <op> want. An unknown op is refused at validate
// time, so reaching the default here would be a programming error, not input.
func metricHolds(got float64, op string, want float64) bool {
	switch op {
	case "<":
		return got < want
	case "<=":
		return got <= want
	case ">":
		return got > want
	case ">=":
		return got >= want
	case "==":
		return got == want
	case "!=":
		return got != want
	default:
		return false
	}
}

func pass(name, format string, a ...any) Result {
	return Result{Name: name, OK: true, Detail: fmt.Sprintf(format, a...)}
}

func fail(name, format string, a ...any) Result {
	return Result{Name: name, OK: false, Detail: fmt.Sprintf(format, a...)}
}
