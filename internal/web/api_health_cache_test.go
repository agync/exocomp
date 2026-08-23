package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/wire"
)

// TestAPIHealthProbeIsCached is the AI-6 regression: GET /api/v1/health used to
// run the full doctor probe per request — eight PATH-resolved subprocess forks
// and a keyring unwrap — for any token including readonly, at whatever rate the
// caller liked. The probe is now cached behind a short TTL, so a flood of reads
// costs one probe, and concurrent readers share it rather than each forking.
func TestAPIHealthProbeIsCached(t *testing.T) {
	h := newHarness(t)
	var probes atomic.Int64
	h.server.probe = func(context.Context) wire.HealthView {
		probes.Add(1)
		return wire.HealthView{{Name: "data-dir", Status: wire.HealthOK}}
	}
	now := time.Now().UTC()
	h.server.clock = func() time.Time { return now }

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")

	for i := 0; i < 10; i++ {
		if code, _, _ := bearerGet(t, ts.URL, "/api/v1/health", ro); code != http.StatusOK {
			t.Fatalf("health status = %d, want 200", code)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bearerGet(t, ts.URL, "/api/v1/health", ro)
		}()
	}
	wg.Wait()
	if got := probes.Load(); got != 1 {
		t.Fatalf("18 health reads ran %d probes, want 1 — each read forks the doctor probe", got)
	}

	// Past the TTL the next read refreshes: the cache is short, not stale.
	now = now.Add(healthCacheTTL + time.Second)
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/health", ro); code != http.StatusOK {
		t.Fatal("health after the TTL did not answer")
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("probes after the TTL = %d, want 2", got)
	}
}

// TestAPIHealthDefaultProbeIsTheDoctorProbe guards the seam: the cached probe
// must still be internal/health.Probe, so /api/v1/health and `doctor --output
// json` cannot diverge.
func TestAPIHealthDefaultProbeIsTheDoctorProbe(t *testing.T) {
	h := newHarness(t)
	got := h.server.probe(context.Background())
	want := health.Probe(context.Background(), h.dir, h.server.healthOpenStore)
	if len(got) != len(want) {
		t.Fatalf("cached probe returned %d entries, doctor's returns %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Name != want[i].Name {
			t.Fatalf("entry %d = %q, doctor's = %q", i, got[i].Name, want[i].Name)
		}
	}
}
