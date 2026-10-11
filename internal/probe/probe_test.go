package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"tcp ok", Spec{Kind: KindTCP, Address: "192.168.1.5", Port: 22}, true},
		{"tcp hostname", Spec{Kind: KindTCP, Address: "web.lan", Port: 22}, false},
		{"tcp port zero", Spec{Kind: KindTCP, Address: "192.168.1.5", Port: 0}, false},
		{"tcp port high", Spec{Kind: KindTCP, Address: "192.168.1.5", Port: 70000}, false},
		{"ping ok", Spec{Kind: KindPing, Address: "192.168.1.1"}, true},
		{"ping hostname", Spec{Kind: KindPing, Address: "gateway.local"}, false},
		{"ping port", Spec{Kind: KindPing, Address: "192.168.1.1", Port: 1}, false},
		{"http ok", Spec{Kind: KindHTTP, URL: "https://web.lan/health"}, true},
		{"http bad scheme", Spec{Kind: KindHTTP, URL: "ftp://web.lan"}, false},
		{"http no host", Spec{Kind: KindHTTP, URL: "http://"}, false},
		{"http expect bad", Spec{Kind: KindHTTP, URL: "http://web.lan", ExpectStatus: 42}, false},
		{"dns ok", Spec{Kind: KindDNS, DNSName: "web.lan"}, true},
		{"dns empty", Spec{Kind: KindDNS, DNSName: " "}, false},
		{"tls ok", Spec{Kind: KindTLS, Address: "web.lan:443", MinDays: 14}, true},
		{"tls no port", Spec{Kind: KindTLS, Address: "web.lan"}, false},
		{"tls no host", Spec{Kind: KindTLS, Address: ":443"}, false},
		{"tls blank host", Spec{Kind: KindTLS, Address: " :443"}, false},
		{"tls bad port", Spec{Kind: KindTLS, Address: "web.lan:0"}, false},
		{"tls negative days", Spec{Kind: KindTLS, Address: "web.lan:443", MinDays: -1}, false},
		{"metric ok", Spec{Kind: KindMetric, URL: "http://web.lan/m", Op: "<", Value: 1}, true},
		{"metric bad op", Spec{Kind: KindMetric, URL: "http://web.lan/m", Op: "~"}, false},
		{"unknown kind", Spec{Kind: "snmp"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestLocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec Spec
		want string
		ok   bool
	}{
		{"tcp literal", Spec{Kind: KindTCP, Address: "192.168.1.5"}, "192.168.1.5", true},
		{"ping literal", Spec{Kind: KindPing, Address: "fd00::1"}, "fd00::1", true},
		{"tcp hostname", Spec{Kind: KindTCP, Address: "web.lan"}, "", false},
		{"tls host:port", Spec{Kind: KindTLS, Address: "web.lan:443"}, "", false},
		{"tls bare ip", Spec{Kind: KindTLS, Address: "192.168.1.5"}, "192.168.1.5", true},
		{"dns name", Spec{Kind: KindDNS, DNSName: "192.168.1.5"}, "", false},
		{"metric url", Spec{Kind: KindMetric, URL: "http://192.168.1.5/"}, "", false},
		{"http url", Spec{Kind: KindHTTP, URL: "http://192.168.1.5/"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, ok := tc.spec.Location()
			if ok != tc.ok {
				t.Fatalf("Location() ok = %v, want %v", ok, tc.ok)
			}
			if ok && addr != netip.MustParseAddr(tc.want) {
				t.Fatalf("Location() = %v, want %v", addr, tc.want)
			}
			if !ok && addr.IsValid() {
				t.Fatalf("Location() returned a valid address with ok=false: %v", addr)
			}
		})
	}
}

func TestRunTCP(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	up := Run(context.Background(), Spec{Kind: KindTCP, Address: host, Port: port},
		Options{Timeout: 2 * time.Second, AllowLoopback: true})
	if !up.Up {
		t.Fatalf("live listener = %+v, want up", up)
	}
	_ = ln.Close()

	downHost, downPort := closedLoopback(t)
	down := Run(context.Background(), Spec{Kind: KindTCP, Address: downHost, Port: downPort}, Options{Timeout: time.Second})
	if down.Up {
		t.Fatalf("closed listener = %+v, want down", down)
	}
	if down.Duration <= 0 {
		t.Fatalf("down.Duration = %v, want positive", down.Duration)
	}
}

// TestRunTCPGuardBothWays pins the tcp half of the address-class guard: a live
// loopback listener is refused (down) when AllowLoopback is false (the check
// path) and answers up when it is true (a gate). Both runs use the
// package-built dialer, not an injected one.
func TestRunTCPGuardBothWays(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	spec := Spec{Kind: KindTCP, Address: host, Port: port}
	ctx := context.Background()

	guarded := Run(ctx, spec, Options{Timeout: 2 * time.Second, AllowLoopback: false})
	if guarded.Up {
		t.Fatalf("loopback tcp with AllowLoopback=false = %+v, want down (refused)", guarded)
	}
	unguarded := Run(ctx, spec, Options{Timeout: 2 * time.Second, AllowLoopback: true})
	if !unguarded.Up {
		t.Fatalf("loopback tcp with AllowLoopback=true = %+v, want up", unguarded)
	}
}

// TestRunTCPGuardRefusesMetadata pins the metadata address (link-local unicast)
// as refused by the tcp guard: the run reads down and names the refusal, so a
// readonly token cannot make the daemon probe the cloud metadata service.
func TestRunTCPGuardRefusesMetadata(t *testing.T) {
	t.Parallel()
	res := Run(context.Background(),
		Spec{Kind: KindTCP, Address: "169.254.169.254", Port: 80},
		Options{Timeout: time.Second})
	if res.Up {
		t.Fatalf("metadata tcp = %+v, want down", res)
	}
	if !strings.Contains(res.Detail, "refusing") {
		t.Fatalf("metadata tcp detail = %q, want a refusal", res.Detail)
	}
}

// closedLoopback binds a loopback listener, closes it, and returns its address
// and port only once a dial is refused. Closing a listener and dialing in the
// same breath can still connect in the kernel's teardown window, so the poll is
// what makes the down case deterministic.
func closedLoopback(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	_ = ln.Close()

	addr := net.JoinHostPort(host, portStr)
	d := net.Dialer{Timeout: 50 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := d.Dial("tcp", addr)
		if derr != nil {
			return host, port
		}
		_ = conn.Close()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("port %s still accepts after the listener closed", addr)
	return "", 0
}

func skipWithoutICMP(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("no ping binary on this host")
	}
	if err := exec.Command("ping", "-c", "1", "-W", "1", "127.0.0.1").Run(); err != nil {
		t.Skipf("host cannot send ICMP (missing cap_net_raw or unprivileged container): %v", err)
	}
}

func TestRunPing(t *testing.T) {
	t.Parallel()
	skipWithoutICMP(t)
	res := Run(context.Background(), Spec{Kind: KindPing, Address: "127.0.0.1"},
		Options{Timeout: 3 * time.Second, AllowLoopback: true})
	if !res.Up {
		t.Fatalf("loopback ping = %+v, want up", res)
	}
}

// TestRunPingGuardBothWays pins the ping half of the address-class guard: the
// loopback is refused before the subprocess runs when AllowLoopback is false,
// and answers when it is true.
func TestRunPingGuardBothWays(t *testing.T) {
	t.Parallel()
	skipWithoutICMP(t)
	spec := Spec{Kind: KindPing, Address: "127.0.0.1"}
	ctx := context.Background()

	guarded := Run(ctx, spec, Options{Timeout: 3 * time.Second, AllowLoopback: false})
	if guarded.Up {
		t.Fatalf("loopback ping with AllowLoopback=false = %+v, want down (refused)", guarded)
	}
	if !strings.Contains(guarded.Detail, "refusing") {
		t.Fatalf("guarded ping detail = %q, want a refusal", guarded.Detail)
	}
	unguarded := Run(ctx, spec, Options{Timeout: 3 * time.Second, AllowLoopback: true})
	if !unguarded.Up {
		t.Fatalf("loopback ping with AllowLoopback=true = %+v, want up", unguarded)
	}
}

// TestHTTPAcceptRange pins the two callers' http defaults: a verify-style spec
// (HTTPAllow3xx false) accepts only 2xx, while a check-style spec
// (HTTPAllow3xx true) accepts 2xx/3xx. An injected client bypasses the dial
// guard so the loopback test server answers.
func TestHTTPAcceptRange(t *testing.T) {
	t.Parallel()
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	opts := Options{Timeout: 2 * time.Second, HTTPClient: client}
	ctx := context.Background()

	verifyStyle := func() Spec { return Spec{Kind: KindHTTP, URL: srv.URL, HTTPAllow3xx: false} }
	checkStyle := func() Spec { return Spec{Kind: KindHTTP, URL: srv.URL, HTTPAllow3xx: true} }

	if res := Run(ctx, verifyStyle(), opts); !res.Up {
		t.Fatalf("200 verify-style = %+v, want up", res)
	}
	code = 301
	if res := Run(ctx, verifyStyle(), opts); res.Up {
		t.Fatalf("301 verify-style = %+v, want down (2xx only)", res)
	}
	if res := Run(ctx, checkStyle(), opts); !res.Up {
		t.Fatalf("301 check-style = %+v, want up (2xx/3xx)", res)
	}
	code = 404
	if res := Run(ctx, checkStyle(), opts); res.Up {
		t.Fatalf("404 check-style = %+v, want down", res)
	}
	code = 200
	if res := Run(ctx, checkStyle(), opts); !res.Up {
		t.Fatalf("200 check-style = %+v, want up", res)
	}
}

// TestHTTPDialGuardBothWays pins the option-gated loopback guard: with
// AllowLoopback false (the check path) a loopback target is refused and reads
// down; with it true (the gate path) the same target answers up. Both runs use
// the package-built client, not an injected one.
func TestHTTPDialGuardBothWays(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := Spec{Kind: KindHTTP, URL: srv.URL}
	ctx := context.Background()

	guarded := Run(ctx, spec, Options{Timeout: 2 * time.Second, AllowLoopback: false})
	if guarded.Up {
		t.Fatalf("loopback with AllowLoopback=false = %+v, want down", guarded)
	}
	unguarded := Run(ctx, spec, Options{Timeout: 2 * time.Second, AllowLoopback: true})
	if !unguarded.Up {
		t.Fatalf("loopback with AllowLoopback=true = %+v, want up", unguarded)
	}
}

func TestRunDNS(t *testing.T) {
	t.Parallel()
	opts := Options{
		LookupHost: func(_ context.Context, host string) ([]string, error) {
			if host == "known.test" {
				return []string{"10.0.0.4", "10.0.0.5"}, nil
			}
			return nil, errors.New("no such host")
		},
	}
	ctx := context.Background()

	if res := Run(ctx, Spec{Kind: KindDNS, DNSName: "known.test"}, opts); !res.Up {
		t.Fatalf("resolving name = %+v, want up", res)
	}
	if res := Run(ctx, Spec{Kind: KindDNS, DNSName: "missing.test"}, opts); res.Up {
		t.Fatalf("unresolvable name = %+v, want down", res)
	}
	if res := Run(ctx, Spec{Kind: KindDNS, DNSName: "known.test", ExpectAddr: "10.0.0.5"}, opts); !res.Up {
		t.Fatalf("expected address = %+v, want up", res)
	}
	if res := Run(ctx, Spec{Kind: KindDNS, DNSName: "known.test", ExpectAddr: "10.0.0.9"}, opts); res.Up {
		t.Fatalf("wrong expected address = %+v, want down", res)
	}
}

func TestRunTLS(t *testing.T) {
	t.Parallel()
	now := time.Now()
	dial := func(_ context.Context, _ string) (tls.ConnectionState, error) {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{
			{NotAfter: now.Add(2 * 24 * time.Hour)},
		}}, nil
	}
	opts := Options{TLSDialer: dial, Now: func() time.Time { return now }}

	if res := Run(context.Background(), Spec{Kind: KindTLS, Address: "web.lan:443", MinDays: 14}, opts); res.Up {
		t.Fatalf("2-day cert against a 14-day floor = %+v, want down", res)
	}
	if res := Run(context.Background(), Spec{Kind: KindTLS, Address: "web.lan:443", MinDays: 1}, opts); !res.Up {
		t.Fatalf("2-day cert against a 1-day floor = %+v, want up", res)
	}
	failing := Options{TLSDialer: func(context.Context, string) (tls.ConnectionState, error) {
		return tls.ConnectionState{}, errors.New("handshake refused")
	}}
	if res := Run(context.Background(), Spec{Kind: KindTLS, Address: "web.lan:443"}, failing); res.Up {
		t.Fatalf("failed handshake = %+v, want down", res)
	}
}

// TestRunTLSGuardBothWays pins the tls half of the address-class guard: a
// literal loopback address is refused before the dialer runs when AllowLoopback
// is false, and handshakes normally when it is true. A hostname target is
// guarded at connect time by the default dialer's Control hook.
func TestRunTLSGuardBothWays(t *testing.T) {
	t.Parallel()
	now := time.Now()
	called := false
	dial := func(_ context.Context, _ string) (tls.ConnectionState, error) {
		called = true
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{
			{NotAfter: now.Add(48 * time.Hour)},
		}}, nil
	}
	spec := Spec{Kind: KindTLS, Address: "127.0.0.1:443", MinDays: 1}
	ctx := context.Background()

	called = false
	guarded := Run(ctx, spec, Options{TLSDialer: dial, AllowLoopback: false})
	if guarded.Up || called {
		t.Fatalf("literal loopback tls with AllowLoopback=false = %+v called=%v, want refused before the dialer", guarded, called)
	}
	called = false
	unguarded := Run(ctx, spec, Options{TLSDialer: dial, AllowLoopback: true})
	if !unguarded.Up || !called {
		t.Fatalf("literal loopback tls with AllowLoopback=true = %+v called=%v, want up through the dialer", unguarded, called)
	}
}

func TestRunMetric(t *testing.T) {
	t.Parallel()
	body := "0.5"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	opts := Options{Timeout: 2 * time.Second, HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	ctx := context.Background()

	if res := Run(ctx, Spec{Kind: KindMetric, URL: srv.URL, Op: "<", Value: 1}, opts); !res.Up {
		t.Fatalf("0.5 < 1 = %+v, want up", res)
	}
	body = "2.5"
	if res := Run(ctx, Spec{Kind: KindMetric, URL: srv.URL, Op: "<", Value: 1}, opts); res.Up {
		t.Fatalf("2.5 < 1 = %+v, want down", res)
	}
	body = "not-a-number"
	if res := Run(ctx, Spec{Kind: KindMetric, URL: srv.URL, Op: "<", Value: 1}, opts); res.Up {
		t.Fatalf("non-numeric body = %+v, want down", res)
	}
}

// TestDialControl pins the guard's address classes directly.
func TestDialControl(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:80", true},
		{"[::1]:80", true},
		{"169.254.169.254:80", true},
		{"169.254.1.1:80", true},
		{"0.0.0.0:80", true},
		{"224.0.0.1:80", true},
		{"192.168.1.50:80", false},
		{"10.0.0.5:80", false},
		{"172.16.0.9:80", false},
		{"[fd00::1]:80", false},
		{"8.8.8.8:80", false},
	} {
		err := dialControl("", tc.addr, nil)
		if (err != nil) != tc.want {
			t.Errorf("dialControl(%q) = %v, want refused=%v", tc.addr, err, tc.want)
		}
	}
}
