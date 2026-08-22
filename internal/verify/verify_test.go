package verify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPProbe(t *testing.T) {
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()

	r := Runner{Timeout: 2 * time.Second}
	ctx := context.Background()

	// A healthy 2xx passes. The companion is the mutation below: the SAME probe
	// against the SAME server fails once the server returns 500, so the pass is
	// not a probe that always passes.
	res := r.Run(ctx, Probe{Name: "up", HTTP: &HTTPProbe{URL: srv.URL}})
	if !res.OK {
		t.Fatalf("expected 200 to pass, got %s", res.Detail)
	}

	code = 500
	res = r.Run(ctx, Probe{Name: "up", HTTP: &HTTPProbe{URL: srv.URL}})
	if res.OK {
		t.Fatalf("expected 500 to FAIL a 2xx gate, but it passed: %s", res.Detail)
	}

	// A declared specific status accepts exactly that code and rejects a 2xx.
	code = 301
	if res = r.Run(ctx, Probe{Name: "redir", HTTP: &HTTPProbe{URL: srv.URL, ExpectStatus: 301}}); !res.OK {
		t.Fatalf("expected explicit 301 to pass, got %s", res.Detail)
	}
	code = 200
	if res = r.Run(ctx, Probe{Name: "redir", HTTP: &HTTPProbe{URL: srv.URL, ExpectStatus: 301}}); res.OK {
		t.Fatalf("expected 200 to fail a 301-expecting probe, but it passed: %s", res.Detail)
	}
}

func TestHTTPProbeUnreachable(t *testing.T) {
	// A gate whose endpoint refuses the connection has FAILED, not errored: the
	// whole point is that an unreachable service is a failed health check.
	r := Runner{Timeout: 500 * time.Millisecond}
	res := r.Run(context.Background(), Probe{Name: "down",
		HTTP: &HTTPProbe{URL: "http://127.0.0.1:1/"}})
	if res.OK {
		t.Fatalf("expected an unreachable endpoint to fail, got pass: %s", res.Detail)
	}
}

func TestDNSProbe(t *testing.T) {
	r := Runner{
		LookupHost: func(_ context.Context, host string) ([]string, error) {
			if host == "known.test" {
				return []string{"10.0.0.4", "10.0.0.5"}, nil
			}
			return nil, errors.New("no such host")
		},
	}
	ctx := context.Background()

	// Resolves at all -> pass; a name that does not resolve -> fail (companion).
	if res := r.Run(ctx, Probe{Name: "dns", DNS: &DNSProbe{Name: "known.test"}}); !res.OK {
		t.Fatalf("expected a resolving name to pass, got %s", res.Detail)
	}
	if res := r.Run(ctx, Probe{Name: "dns", DNS: &DNSProbe{Name: "missing.test"}}); res.OK {
		t.Fatalf("expected an unresolvable name to fail, got pass: %s", res.Detail)
	}

	// A cutover verified by value: the expected address must be among them.
	if res := r.Run(ctx, Probe{Name: "cut", DNS: &DNSProbe{Name: "known.test", Expect: "10.0.0.5"}}); !res.OK {
		t.Fatalf("expected the expected address to pass, got %s", res.Detail)
	}
	if res := r.Run(ctx, Probe{Name: "cut", DNS: &DNSProbe{Name: "known.test", Expect: "10.0.0.9"}}); res.OK {
		t.Fatalf("expected a wrong expected address to fail, got pass: %s", res.Detail)
	}
}

func TestTLSProbeNotAfter(t *testing.T) {
	// A cert two days from expiry, served by a real TLS listener the probe
	// handshakes against. min_days=14 must fail it; min_days=1 must pass the
	// SAME endpoint — so the failure is the expiry floor, not a broken dial.
	addr, stop := tlsServer(t, 2*24*time.Hour)
	defer stop()

	r := Runner{Timeout: 3 * time.Second}
	ctx := context.Background()

	res := r.Run(ctx, Probe{Name: "cert", TLS: &TLSProbe{Address: addr, MinDays: 14}})
	if res.OK {
		t.Fatalf("expected a 2-day cert to fail a 14-day floor, got pass: %s", res.Detail)
	}
	res = r.Run(ctx, Probe{Name: "cert", TLS: &TLSProbe{Address: addr, MinDays: 1}})
	if !res.OK {
		t.Fatalf("expected a 2-day cert to pass a 1-day floor, got %s", res.Detail)
	}
}

func TestTLSProbeHandshakeFails(t *testing.T) {
	// A plain TCP listener speaks no TLS: the handshake fails, and so does the
	// probe. The positive companion is TestTLSProbeNotAfter's passing case.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	r := Runner{Timeout: 2 * time.Second}
	res := r.Run(context.Background(), Probe{Name: "cert",
		TLS: &TLSProbe{Address: ln.Addr().String(), MinDays: 1}})
	if res.OK {
		t.Fatalf("expected a non-TLS endpoint to fail the handshake, got pass: %s", res.Detail)
	}
}

func TestMetricProbe(t *testing.T) {
	body := "0.5"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	r := Runner{Timeout: 2 * time.Second}
	ctx := context.Background()

	// 0.5 < 1 passes; the same probe fails once the body crosses the threshold.
	if res := r.Run(ctx, Probe{Name: "lag", Metric: &MetricProbe{URL: srv.URL, Op: "<", Value: 1}}); !res.OK {
		t.Fatalf("expected 0.5 < 1 to pass, got %s", res.Detail)
	}
	body = "2.5"
	if res := r.Run(ctx, Probe{Name: "lag", Metric: &MetricProbe{URL: srv.URL, Op: "<", Value: 1}}); res.OK {
		t.Fatalf("expected 2.5 < 1 to fail, got pass: %s", res.Detail)
	}
	body = "not-a-number"
	if res := r.Run(ctx, Probe{Name: "lag", Metric: &MetricProbe{URL: srv.URL, Op: "<", Value: 1}}); res.OK {
		t.Fatalf("expected a non-numeric body to fail, got pass: %s", res.Detail)
	}
}

func TestRunAllNamesFailures(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer down.Close()

	var lines []string
	r := Runner{Timeout: 2 * time.Second}
	ok, failed := r.RunAll(context.Background(), []Probe{
		{Name: "web", HTTP: &HTTPProbe{URL: up.URL}},
		{Name: "api", HTTP: &HTTPProbe{URL: down.URL}},
	}, func(l string) { lines = append(lines, l) })

	if ok {
		t.Fatal("expected the gate to fail when one probe fails")
	}
	if len(failed) != 1 || failed[0] != "api" {
		t.Fatalf("expected the failing probe named api, got %v", failed)
	}
	// The passing probe must be reported too, so an all-pass gate is not just an
	// empty run that reports nothing.
	if len(lines) != 2 {
		t.Fatalf("expected one line per probe, got %d: %v", len(lines), lines)
	}

	// All-pass companion: the same machinery returns ok with no failures.
	ok, failed = r.RunAll(context.Background(), []Probe{
		{Name: "web", HTTP: &HTTPProbe{URL: up.URL}},
	}, nil)
	if !ok || failed != nil {
		t.Fatalf("expected an all-pass gate to pass, got ok=%v failed=%v", ok, failed)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	probes := []Probe{
		{Name: "web", HTTP: &HTTPProbe{URL: "http://127.0.0.1:8080/health", ExpectStatus: 200}},
		{Name: "cert", TLS: &TLSProbe{Address: "example.test:443", MinDays: 14}},
	}
	argv, err := Encode(probes)
	if err != nil {
		t.Fatal(err)
	}
	if !IsGateArgv(argv) {
		t.Fatal("encoded argv is not recognised as a gate argv")
	}
	back, err := Decode(argv)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Name != "web" || back[1].TLS.MinDays != 14 {
		t.Fatalf("round-trip lost data: %+v", back)
	}

	// Mutation: a corrupted payload must be refused, not silently probe nothing.
	if _, err := Decode([]string{gateArgv0, "!!!not-base64"}); err == nil {
		t.Fatal("expected a corrupted gate payload to be refused")
	}
}

func TestValidateAllRefusesBadGates(t *testing.T) {
	cases := []struct {
		name   string
		probes []Probe
	}{
		{"empty", nil},
		{"no-check", []Probe{{Name: "x"}}},
		{"no-name", []Probe{{HTTP: &HTTPProbe{URL: "http://h/"}}}},
		{"two-checks", []Probe{{Name: "x", HTTP: &HTTPProbe{URL: "http://h/"}, DNS: &DNSProbe{Name: "h"}}}},
		{"dup-name", []Probe{
			{Name: "x", HTTP: &HTTPProbe{URL: "http://h/"}},
			{Name: "x", DNS: &DNSProbe{Name: "h"}},
		}},
		{"bad-url", []Probe{{Name: "x", HTTP: &HTTPProbe{URL: "ftp://h/"}}}},
		{"bad-op", []Probe{{Name: "x", Metric: &MetricProbe{URL: "http://h/", Op: "~", Value: 1}}}},
		{"tls-no-port", []Probe{{Name: "x", TLS: &TLSProbe{Address: "host"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAll(tc.probes); err == nil {
				t.Fatalf("expected %s to be refused", tc.name)
			}
		})
	}

	// Companion: a well-formed gate is accepted, so ValidateAll is not just
	// rejecting everything.
	if err := ValidateAll([]Probe{{Name: "ok", HTTP: &HTTPProbe{URL: "http://h/"}}}); err != nil {
		t.Fatalf("expected a valid gate to pass, got %v", err)
	}
}

// tlsServer stands up a TLS listener with a self-signed leaf whose not-after is
// validFor from now, and returns its address and a stop function.
func tlsServer(t *testing.T, validFor time.Duration) (string, func()) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "probe.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validFor),
		DNSNames:     []string{"probe.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Force the handshake, then close.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}
