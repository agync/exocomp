package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// tlsServer starts an httptest TLS server with handler h and returns it plus the
// SHA-256 fingerprint of its leaf certificate, computed the same way the client
// pins — so a test can pin the real cert or a deliberately wrong one.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	cert := srv.Certificate()
	return srv, Fingerprint(cert.Raw)
}

func newTestClient(t *testing.T, srv *httptest.Server, fp string) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: srv.URL, Fingerprint: fp, Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestListJobsDecodesEnvelope(t *testing.T) {
	exit := 0
	want := []wire.JobView{
		{ID: 1, State: "succeeded", Runbook: "web", ExitCode: &exit, QueuedAt: "2026-08-22T10:00:00Z"},
		{ID: 2, State: "running", Runbook: "db", QueuedAt: "2026-08-22T10:01:00Z"},
	}
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer", got)
		}
		if r.URL.Path != "/api/v1/jobs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("state") != "running" {
			t.Errorf("state filter not forwarded: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindJobList, want)
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.ListJobs(context.Background(), JobFilter{States: []string{"running"}})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].State != "running" {
		t.Fatalf("decoded jobs mismatch: %+v", got)
	}
}

// The decoded value re-emitted through wire.WriteSuccess must be byte-identical
// to what the server wrote: that is the guarantee that remote --output json
// matches local --output json.
func TestRemoteBytesMatchLocal(t *testing.T) {
	rows := []wire.JobView{{ID: 7, State: "queued", Runbook: "web", QueuedAt: "2026-08-22T10:00:00Z"}}
	serverBytes, err := wire.MarshalSuccess(wire.KindJobList, rows)
	if err != nil {
		t.Fatal(err)
	}
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(serverBytes)
	}))
	c := newTestClient(t, srv, fp)
	got, err := c.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	reEmitted, err := wire.MarshalSuccess(wire.KindJobList, got)
	if err != nil {
		t.Fatal(err)
	}
	if string(reEmitted) != string(serverBytes) {
		t.Fatalf("re-emitted bytes differ from server:\n server: %s\n client: %s", serverBytes, reEmitted)
	}
}

func TestErrorEnvelopeBecomesTypedError(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeNotFound, "no such job"))
	}))
	c := newTestClient(t, srv, fp)
	_, err := c.GetJob(context.Background(), 999)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != wire.CodeNotFound {
		t.Errorf("code = %q, want %q", apiErr.Code, wire.CodeNotFound)
	}
	if apiErr.ExitCode() != wire.ExitNotFound {
		t.Errorf("exit = %d, want %d", apiErr.ExitCode(), wire.ExitNotFound)
	}
}

func TestPinMismatchRefused(t *testing.T) {
	srv, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = wire.WriteSuccess(w, wire.KindJobList, []wire.JobView{})
	}))
	// Pin a fingerprint that is NOT the server's — a stand-in for a MITM cert.
	wrong := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	c := newTestClient(t, srv, wrong)
	_, err := c.ListJobs(context.Background(), JobFilter{})
	var pin *PinMismatchError
	if !errors.As(err, &pin) {
		t.Fatalf("want *PinMismatchError, got %T: %v", err, err)
	}
}

func TestFetchFingerprintMatchesServer(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	got, err := FetchFingerprint(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchFingerprint: %v", err)
	}
	if !sameFingerprint(got, fp) {
		t.Fatalf("fingerprint = %q, want %q", got, fp)
	}
}

func TestFingerprintMatchesStdlibFormat(t *testing.T) {
	// A fixed DER-less check: Fingerprint of empty input is the sha256 of empty,
	// colon-grouped uppercase hex — the exact shape web/tls.go emits.
	got := Fingerprint(nil)
	const wantEmpty = "E3:B0:C4:42:98:FC:1C:14:9A:FB:F4:C8:99:6F:B9:24:27:AE:41:E4:64:9B:93:4C:A4:95:99:1B:78:52:B8:55"
	if got != wantEmpty {
		t.Fatalf("Fingerprint(nil) = %q, want %q", got, wantEmpty)
	}
}

func TestNewRejectsHTTP(t *testing.T) {
	_, err := New(Config{Endpoint: "http://box:8080", Fingerprint: "AA", Token: "t"})
	if err == nil {
		t.Fatal("want refusal of http endpoint")
	}
}

func TestNewRequiresPinAndToken(t *testing.T) {
	if _, err := New(Config{Endpoint: "box", Token: "t"}); err == nil {
		t.Error("want error with no fingerprint")
	}
	if _, err := New(Config{Endpoint: "box", Fingerprint: "AA"}); err == nil {
		t.Error("want error with no token")
	}
}

// guard the pinned client actually presents InsecureSkipVerify with a
// VerifyConnection hook, so a future refactor cannot silently drop the pin.
func TestTransportPinsViaVerifyConnection(t *testing.T) {
	c, err := New(Config{Endpoint: "box", Fingerprint: "AA", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.hc.Transport)
	}
	cfg := tr.TLSClientConfig
	if cfg == nil || !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil {
		t.Fatalf("expected InsecureSkipVerify with a VerifyConnection pin, got %+v", cfg)
	}
	// And the hook rejects an empty peer set rather than defaulting open.
	if err := c.verifyPin(tls.ConnectionState{}); err == nil {
		t.Error("verifyPin admitted an empty certificate chain")
	}
}
