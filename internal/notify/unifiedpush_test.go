package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// fakeRegistry is an in-memory PushRegistry recording what the transport did.
type fakeRegistry struct {
	mu        sync.Mutex
	rows      []store.PushEndpoint
	succeeded []int64
	errored   map[int64]string
	deleted   []int64
}

func newFakeRegistry(rows ...store.PushEndpoint) *fakeRegistry {
	return &fakeRegistry{rows: rows, errored: map[int64]string{}}
}

func (f *fakeRegistry) ActivePushEndpoints(context.Context) ([]store.PushEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.PushEndpoint(nil), f.rows...), nil
}

func (f *fakeRegistry) MarkPushEndpointSuccess(_ context.Context, id int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded = append(f.succeeded, id)
	return nil
}

func (f *fakeRegistry) MarkPushEndpointError(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errored[id] = msg
	return nil
}

func (f *fakeRegistry) DeletePushEndpoint(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

// plainTransport builds a transport whose client has no SSRF guard, so it can
// reach the loopback test servers the fan-out cases stand up. The guard itself
// is proven separately (TestUnifiedPushDialGuardRefusesLoopback).
func plainTransport(reg PushRegistry) *UnifiedPush {
	return &UnifiedPush{
		reg:    reg,
		client: &http.Client{Timeout: 2 * time.Second},
		log:    slog.Default(),
		now:    func() time.Time { return time.Unix(0, 0).UTC() },
	}
}

func TestUnifiedPushFansOutAndIsolatesFailures(t *testing.T) {
	ctx := context.Background()
	var hitsOK, hitsBad int
	var muHits sync.Mutex

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muHits.Lock()
		hitsOK++
		muHits.Unlock()
		var msg pushMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Title != "1 approval waiting" {
			t.Errorf("endpoint got title %q, want the notification's own", msg.Title)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		muHits.Lock()
		hitsBad++
		muHits.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()

	reg := newFakeRegistry(
		store.PushEndpoint{ID: 1, URL: badSrv.URL}, // fails first
		store.PushEndpoint{ID: 2, URL: okSrv.URL},  // must still be reached
	)
	tr := plainTransport(reg)

	if err := tr.Publish(ctx, Notification{Title: "1 approval waiting", Body: "tap to review", Priority: PriorityMax}); err != nil {
		t.Fatalf("Publish returned %v, want nil (per-endpoint failures are isolated)", err)
	}
	if hitsOK != 1 || hitsBad != 1 {
		t.Fatalf("hits ok=%d bad=%d, want 1 and 1 (a failing endpoint must not block the other)", hitsOK, hitsBad)
	}
	if len(reg.succeeded) != 1 || reg.succeeded[0] != 2 {
		t.Fatalf("succeeded = %v, want [2]", reg.succeeded)
	}
	if _, ok := reg.errored[1]; !ok {
		t.Fatalf("endpoint 1 should have recorded an error, errored = %v", reg.errored)
	}
}

func TestUnifiedPushPrunesOn410(t *testing.T) {
	ctx := context.Background()
	goneSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer goneSrv.Close()
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFoundSrv.Close()

	reg := newFakeRegistry(
		store.PushEndpoint{ID: 7, URL: goneSrv.URL},
		store.PushEndpoint{ID: 9, URL: notFoundSrv.URL},
	)
	tr := plainTransport(reg)
	if err := tr.Publish(ctx, Notification{Title: "x", Body: "y"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(reg.deleted) != 2 {
		t.Fatalf("deleted = %v, want both dropped endpoints pruned", reg.deleted)
	}
}

func TestUnifiedPushNoEndpointsIsNoOp(t *testing.T) {
	reg := newFakeRegistry()
	tr := plainTransport(reg)
	if err := tr.Publish(context.Background(), Notification{Title: "x"}); err != nil {
		t.Fatalf("Publish with no endpoints = %v, want nil no-op", err)
	}
}

// TestUnifiedPushDialGuardRefusesLoopback is the send-time SSRF proof: the
// guarded client NewUnifiedPush builds refuses to connect to a server on a
// loopback address, and the server never receives the request. The refusal
// happens on the concrete dialed IP, which is what defeats DNS rebinding.
func TestUnifiedPushDialGuardRefusesLoopback(t *testing.T) {
	ctx := context.Background()
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := newFakeRegistry(store.PushEndpoint{ID: 1, URL: srv.URL}) // 127.0.0.1:port
	tr := NewUnifiedPush(reg, nil)
	if err := tr.Publish(ctx, Notification{Title: "x"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if hit != 0 {
		t.Fatalf("guarded client reached a loopback server %d times, want 0", hit)
	}
	if _, ok := reg.errored[1]; !ok {
		t.Fatalf("loopback endpoint should have recorded a dial refusal, errored = %v", reg.errored)
	}
}

// TestDialControlPredicate exercises the Control hook directly on concrete
// addresses, the address-level check registration alone cannot make.
func TestDialControlPredicate(t *testing.T) {
	refused := []string{
		"127.0.0.1:443",
		"10.0.0.5:443",
		"192.168.1.10:443",
		"172.16.5.5:443",
		"169.254.169.254:80", // cloud metadata
		"169.254.1.1:443",    // link-local
		"[::1]:443",
		"[fe80::1]:443",
		"[fc00::1]:443",
		"0.0.0.0:443",
	}
	for _, addr := range refused {
		if err := dialControl("tcp", addr, nil); err == nil {
			t.Errorf("dialControl(%q) = nil, want refused", addr)
		}
	}
	allowed := []string{"1.1.1.1:443", "8.8.8.8:443", "[2606:4700:4700::1111]:443"}
	for _, addr := range allowed {
		if err := dialControl("tcp", addr, nil); err != nil {
			t.Errorf("dialControl(%q) = %v, want allowed", addr, err)
		}
	}
}

// TestValidatePushEndpoint proves the registration-time gate: https-only,
// no credentials, and every resolved address must be public. A hostname that
// resolves to a private IP is refused even though its literal form is a name.
func TestValidatePushEndpoint(t *testing.T) {
	ctx := context.Background()

	bad := []string{
		"http://up.example.com/x",      // not https
		"https://127.0.0.1/x",          // literal loopback
		"https://10.0.0.5/x",           // literal private
		"https://[::1]/x",              // literal ipv6 loopback
		"https://169.254.169.254/x",    // metadata
		"https://user:pw@up.example/x", // credentials in URL
		"ftp://up.example.com/x",       // wrong scheme
	}
	for _, raw := range bad {
		if _, err := ValidatePushEndpoint(ctx, raw); err == nil {
			t.Errorf("ValidatePushEndpoint(%q) = nil, want refused", raw)
		}
	}

	// A DNS host that resolves to a private IP is refused at registration too.
	restore := lookupIP
	defer func() { lookupIP = restore }()
	lookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.1.2.3")}, nil
	}
	if _, err := ValidatePushEndpoint(ctx, "https://rebind.example.com/x"); err == nil {
		t.Fatal("a host resolving to a private IP must be refused at registration")
	}

	// The same host resolving to a public IP is accepted.
	lookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	u, err := ValidatePushEndpoint(ctx, "https://good.example.com/x")
	if err != nil {
		t.Fatalf("public host refused: %v", err)
	}
	if u.Host != "good.example.com" {
		t.Fatalf("parsed host = %q", u.Host)
	}
}
