package reactive

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestCheckHTTPProbeDialControl pins the dial-time guard on the address classes
// a check must not reach — loopback, link-local (the cloud metadata address),
// unspecified, and multicast — while keeping the RFC1918/ULA LAN targets the
// feature exists for.
func TestCheckHTTPProbeDialControl(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr string
		want bool // want error (refused)
	}{
		{"127.0.0.1:80", true},
		{"[::1]:80", true},
		{"169.254.169.254:80", true},
		{"169.254.1.1:80", true},
		{"0.0.0.0:80", true},
		{"224.0.0.1:80", true},
		// LAN and public stays reachable.
		{"192.168.1.50:80", false},
		{"10.0.0.5:80", false},
		{"172.16.0.9:80", false},
		{"[fd00::1]:80", false},
		{"8.8.8.8:80", false},
	} {
		err := probeHTTPDialControl("", tc.addr, nil)
		if (err != nil) != tc.want {
			t.Errorf("probeHTTPDialControl(%q) = %v, want refused=%v", tc.addr, err, tc.want)
		}
	}
}

// TestCheckHTTPProbeReachesValidTarget exercises the successful-GET half of
// probeHTTP (the loopback-refusal sweep test covers the guarded path): with the
// client swapped for an unguarded one, a 200 target is up and a mismatched
// status is down.
func TestCheckHTTPProbeReachesValidTarget(t *testing.T) {
	orig := probeHTTPClient
	defer func() { probeHTTPClient = orig }()
	probeHTTPClient = func() *http.Client { return &http.Client{Timeout: probeTimeout} }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx := context.Background()
	up, err := probeHTTP(ctx, store.CheckTarget{Name: "w", URL: srv.URL + "/ok", Expect: 200})
	if err != nil || !up {
		t.Fatalf("200 target = %v, %v; want up", up, err)
	}
	up, err = probeHTTP(ctx, store.CheckTarget{Name: "w", URL: srv.URL + "/down", Expect: 200})
	if err != nil || up {
		t.Fatalf("503 target = %v, %v; want down", up, err)
	}
}

// TestCheckHTTPEventCarriesNoNetgroupLabel pins the http carve-out at the
// label-builder level: a URL target's liveness is not a location verdict, so
// its event labels carry no netgroup key, while an address target is classified
// (here into the "home" group, or "away" when no group matches).
func TestCheckHTTPEventCarriesNoNetgroupLabel(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: time.Hour,
		Logger:       slog.New(slog.DiscardHandler),
	})
	e, err := New(Options{
		Store:    st,
		Queue:    q,
		Deriver:  stubDeriver{},
		Notifier: notify.New(slog.New(slog.DiscardHandler), discardTransport{}),
		Clock:    func() time.Time { return time.Now().UTC() },
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	if _, err := st.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	httpLabels, err := e.checkLabels(ctx, store.Check{Name: "web"},
		store.CheckTarget{Name: "w", URL: "http://web.lan/health"})
	if err != nil {
		t.Fatalf("http labels: %v", err)
	}
	if _, ok := httpLabels["netgroup"]; ok {
		t.Fatalf("http event carries a netgroup label: %+v", httpLabels)
	}

	homeLabels, err := e.checkLabels(ctx, store.Check{Name: "at-home"},
		store.CheckTarget{Name: "laptop", Address: "192.168.1.50"})
	if err != nil {
		t.Fatalf("home labels: %v", err)
	}
	if homeLabels["netgroup"] != "home" {
		t.Fatalf("home target netgroup = %q, want home", homeLabels["netgroup"])
	}

	awayLabels, err := e.checkLabels(ctx, store.Check{Name: "roam"},
		store.CheckTarget{Name: "laptop", Address: "10.9.9.9"})
	if err != nil {
		t.Fatalf("away labels: %v", err)
	}
	if awayLabels["netgroup"] != "away" {
		t.Fatalf("unmatched target netgroup = %q, want away", awayLabels["netgroup"])
	}
}
