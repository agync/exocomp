package verify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/probe"
)

// Result is one probe's outcome. OK is the only thing that decides the gate;
// Detail explains it either way, for the event stream the daemon redacts.
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Runner performs probes. The zero value is usable and uses the process
// defaults; tests inject their own client, resolver, and clock.
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

// Run performs one probe and returns its result. An error reaching the endpoint
// is a failed probe, not a Run error: a gate whose target is unreachable has
// failed, and returning that as OK=false is exactly the point. The shared probe
// package decides it; a gate allows the loopback classes a check refuses.
func (r Runner) Run(ctx context.Context, p Probe) Result {
	spec, ok := specForProbe(p)
	if !ok {
		return Result{Name: p.Name, OK: false, Detail: "probe declares no check"}
	}
	res := probe.Run(ctx, spec, probe.Options{
		Timeout:       r.Timeout,
		AllowLoopback: true,
		HTTPClient:    r.HTTPClient,
		Resolver:      r.Resolver,
		LookupHost:    r.LookupHost,
		TLSDialer:     r.TLSDialer,
		Now:           r.Now,
	})
	return Result{Name: p.Name, OK: res.Up, Detail: res.Detail}
}

// specForProbe maps verify's tagged union onto the shared flat probe.Spec.
// HTTPAllow3xx stays false: a verify gate's default is any 2xx only, so a
// redirect fails a gate's "the service answered healthily" check while a check
// calls the same status up.
func specForProbe(p Probe) (probe.Spec, bool) {
	switch {
	case p.HTTP != nil:
		return probe.Spec{Kind: probe.KindHTTP, URL: p.HTTP.URL, ExpectStatus: p.HTTP.ExpectStatus}, true
	case p.DNS != nil:
		return probe.Spec{Kind: probe.KindDNS, DNSName: p.DNS.Name, ExpectAddr: p.DNS.Expect}, true
	case p.TLS != nil:
		return probe.Spec{Kind: probe.KindTLS, Address: p.TLS.Address, MinDays: p.TLS.MinDays}, true
	case p.Metric != nil:
		return probe.Spec{Kind: probe.KindMetric, URL: p.Metric.URL, Op: p.Metric.Op, Value: p.Metric.Value}, true
	default:
		return probe.Spec{}, false
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
