package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// maxMetricBody caps how much of a metric endpoint's body is read before it is
// parsed as a number. A readiness gauge is a handful of bytes; anything past
// this is not the number the probe was pointed at.
const maxMetricBody = 4 << 10

// Run performs one probe and returns its result. An error reaching the endpoint
// is a failed probe, not a Run error: a probe whose target is unreachable has
// failed, and returning that as Up=false is exactly the point. Duration is the
// wall-clock time the run took.
func Run(ctx context.Context, s Spec, o Options) Result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, o.timeout())
	defer cancel()
	res := run(ctx, s, o)
	res.Duration = time.Since(start)
	return res
}

func run(ctx context.Context, s Spec, o Options) Result {
	switch s.Kind {
	case KindTCP:
		return runTCP(ctx, s, o)
	case KindPing:
		return runPing(ctx, s, o)
	case KindHTTP:
		return runHTTP(ctx, s, o)
	case KindDNS:
		return runDNS(ctx, s, o)
	case KindTLS:
		return runTLS(ctx, s, o)
	case KindMetric:
		return runMetric(ctx, s, o)
	default:
		return fail("unknown kind %q", s.Kind)
	}
}

// runTCP dials the address:port. Any dial failure — refused, timeout,
// unroutable — is down; the address is the location, and an unreachable
// location answers nothing.
func runTCP(ctx context.Context, s Spec, o Options) Result {
	addr := net.JoinHostPort(s.Address, strconv.Itoa(s.Port))
	d := net.Dialer{}
	if addrGuarded(o) {
		d.Control = dialControl
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("dial %s: %v", addr, err)
	}
	_ = conn.Close()
	return pass("dial %s ok", addr)
}

// runPing runs one ICMP probe and reads the exit code only. -W 2 bounds one
// probe the way Options.Timeout bounds a dial, so a wedged ICMP path reports
// down instead of hanging. The address is a bare literal, so the guard is
// applied directly rather than through a dialer's Control hook.
func runPing(ctx context.Context, s Spec, o Options) Result {
	if addrGuarded(o) {
		if err := refuseAddrLiteral(s.Address); err != nil {
			return fail("ping %s: %v", s.Address, err)
		}
	}
	if err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "2", s.Address).Run(); err != nil {
		return fail("ping %s: %v", s.Address, err)
	}
	return pass("ping %s ok", s.Address)
}

// runHTTP GETs the URL and reads the status line only. The body is drained and
// discarded: a health endpoint's response is not evidence this code should
// persist. ExpectStatus 0 accepts 2xx always, and 3xx too when the caller
// declared it (the check path); otherwise the status must match exactly.
func runHTTP(ctx context.Context, s Spec, o Options) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return fail("build request: %v", err)
	}
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return fail("GET %s: %v", s.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetricBody))

	want := "2xx"
	if s.HTTPAllow3xx {
		want = "2xx/3xx"
	}
	if s.ExpectStatus == 0 {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 ||
			(s.HTTPAllow3xx && resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return pass("GET %s -> %d", s.URL, resp.StatusCode)
		}
		return fail("GET %s -> %d, want %s", s.URL, resp.StatusCode, want)
	}
	if resp.StatusCode == s.ExpectStatus {
		return pass("GET %s -> %d", s.URL, resp.StatusCode)
	}
	return fail("GET %s -> %d, want %d", s.URL, resp.StatusCode, s.ExpectStatus)
}

// runDNS resolves the name. When ExpectAddr is set, one of the resolved
// addresses must equal it — a DNS cutover is verified by the value it now
// points at, not merely by resolving to something.
func runDNS(ctx context.Context, s Spec, o Options) Result {
	lookup := o.LookupHost
	if lookup == nil {
		res := o.Resolver
		if res == nil {
			res = net.DefaultResolver
		}
		lookup = res.LookupHost
	}
	addrs, err := lookup(ctx, s.DNSName)
	if err != nil {
		return fail("resolve %s: %v", s.DNSName, err)
	}
	if len(addrs) == 0 {
		return fail("resolve %s: no addresses", s.DNSName)
	}
	if s.ExpectAddr == "" {
		return pass("%s -> %s", s.DNSName, strings.Join(addrs, ","))
	}
	if slices.Contains(addrs, s.ExpectAddr) {
		return pass("%s -> %s (has %s)", s.DNSName, strings.Join(addrs, ","), s.ExpectAddr)
	}
	return fail("%s -> %s, want %s", s.DNSName, strings.Join(addrs, ","), s.ExpectAddr)
}

// runTLS handshakes the address and checks the leaf's not-after against
// MinDays. The handshake proves the endpoint serves TLS; chain validity is
// deliberately not required, because an internal or self-signed endpoint is a
// legitimate target and the expiry floor is the property under test.
func runTLS(ctx context.Context, s Spec, o Options) Result {
	guarded := addrGuarded(o)
	if guarded {
		if err := guardTLSAddress(s.Address); err != nil {
			return fail("handshake %s: %v", s.Address, err)
		}
	}
	dial := o.TLSDialer
	if dial == nil {
		dial = func(ctx context.Context, address string) (tls.ConnectionState, error) {
			return defaultTLSDial(ctx, guarded, address)
		}
	}
	state, err := dial(ctx, s.Address)
	if err != nil {
		return fail("handshake %s: %v", s.Address, err)
	}
	if len(state.PeerCertificates) == 0 {
		return fail("handshake %s: peer sent no certificate", s.Address)
	}
	leaf := state.PeerCertificates[0]
	days := int(leaf.NotAfter.Sub(o.now()).Hours() / 24)
	if s.MinDays > 0 && days < s.MinDays {
		return fail("%s cert expires in %dd (not-after %s), want >= %dd",
			s.Address, days, leaf.NotAfter.UTC().Format(time.RFC3339), s.MinDays)
	}
	return pass("%s handshake ok, cert valid %dd (not-after %s)",
		s.Address, days, leaf.NotAfter.UTC().Format(time.RFC3339))
}

// defaultTLSDial performs a real handshake and returns the connection state.
// Verification is disabled on purpose: the not-after floor is what the tls
// probe asserts, and an internal or self-signed endpoint is a valid target.
// When guarded, the handshake dials through the same Control hook the http
// client uses, so a hostname re-pointed at a refused class is caught at the
// connected IP.
func defaultTLSDial(ctx context.Context, guarded bool, address string) (tls.ConnectionState, error) {
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // not-after floor is the check, not chain validity
	if guarded {
		d.NetDialer = &net.Dialer{Control: dialControl}
	}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState(), nil
}

// runMetric GETs the URL, parses the body as a number, and evaluates Op against
// Value — the "an exit code is not enough, read the number the service reports"
// check.
func runMetric(ctx context.Context, s Spec, o Options) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return fail("build request: %v", err)
	}
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return fail("GET %s: %v", s.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricBody))
	if err != nil {
		return fail("read %s: %v", s.URL, err)
	}
	got, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil {
		return fail("%s body %q is not a number", s.URL, strings.TrimSpace(string(body)))
	}
	if metricHolds(got, s.Op, s.Value) {
		return pass("%s = %v %s %v", s.URL, got, s.Op, s.Value)
	}
	return fail("%s = %v, want %s %v", s.URL, got, s.Op, s.Value)
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

// httpClient returns the client http and metric probes dial through. When none
// was injected it builds one, guarded unless AllowLoopback says otherwise.
func (o Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	c := &http.Client{Timeout: o.timeout()}
	if !o.AllowLoopback {
		c.Transport = &http.Transport{
			DialContext: (&net.Dialer{Timeout: o.timeout(), Control: dialControl}).DialContext,
		}
	}
	return c
}

// dialControl is the net.Dialer.Control hook every guarded dial goes through,
// mirroring the push transport's dial-time SSRF check (notify/ssrf.go). It
// refuses the addresses a check must not reach at the concrete post-DNS IP, so
// a re-pointed name is blocked too. Unlike the push guard it keeps RFC1918 and
// ULA LAN targets allowed: probing the LAN is the point of a check.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("probe: refusing malformed dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("probe: dial address %q is not a resolved IP", host)
	}
	return refuseAddrClass(ip)
}

// refuseAddrClass is the one address-class deny-list the guard applies: the
// daemon's own loopback, the cloud metadata service (link-local unicast), and
// unspecified or multicast. The metadata address 169.254.169.254 is
// link-local, so IsLinkLocalUnicast refuses it without naming it. RFC1918 and
// ULA are deliberately allowed.
func refuseAddrClass(ip net.IP) error {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("probe: refusing %s: not a reachable LAN address", ip)
	}
	return nil
}

// refuseAddrLiteral applies refuseAddrClass to a bare address literal. A
// non-literal (a hostname) is left nil: it resolves at connect time, where the
// Control hook guards the connected IP.
func refuseAddrLiteral(address string) error {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return nil
	}
	return refuseAddrClass(ip)
}

// guardTLSAddress refuses a tls address whose literal host is in a refused
// class. A hostname passes here and is guarded at connect time by
// defaultTLSDial's Control hook.
func guardTLSAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("probe: refusing malformed tls address %q", address)
	}
	return refuseAddrLiteral(host)
}

// addrGuardOff counts test calls that disabled the address-literal guard. The
// shipped sweep refuses the classes for tcp, ping, and tls; the sweep's tests
// have no live target but a loopback listener, so they hold this open for the
// life of the test. It is zero in production, and the guard itself is pinned by
// this package's own tests.
var addrGuardOff atomic.Int64

// DisableAddrGuardForTest disables the address-class guard for tcp, ping, and
// tls until the returned func runs. http and metric keep their client seam. It
// is inert outside a test binary, so a production build can never lift the
// guard by calling it.
func DisableAddrGuardForTest() (restore func()) {
	if !testing.Testing() {
		return func() {}
	}
	addrGuardOff.Add(1)
	return func() { addrGuardOff.Add(-1) }
}

// addrGuarded reports whether this run's address-class guard applies: the
// caller left AllowLoopback false (a check) and no test holds it open.
func addrGuarded(o Options) bool { return !o.AllowLoopback && addrGuardOff.Load() == 0 }

func pass(format string, a ...any) Result {
	return Result{Up: true, Detail: fmt.Sprintf(format, a...)}
}

func fail(format string, a ...any) Result {
	return Result{Up: false, Detail: fmt.Sprintf(format, a...)}
}
