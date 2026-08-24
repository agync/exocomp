package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestKillRefusesUnauthenticatedAndUnderScoped is the red-team repro for the
// approval page's "Silence & global kill" form (view_approval.templ, action
// "/kill"): the control is destructive (it pauses the reactive engine, the
// dashboard's PAUSED state), so it must sit behind the SAME operator floor as
// the rest of the write surface (canWrite, api.go), not merely behind login.
// Both a caller with no session and a caller holding a readonly or approve
// token must be refused AND must leave no global silence behind — the mutation
// check (security-red-blue) reverts the scope check and watches this fail.
func TestKillRefusesUnauthenticatedAndUnderScoped(t *testing.T) {
	ctx := context.Background()

	t.Run("no session or bearer is refused and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()

		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.PostForm(ts.URL+"/kill", url.Values{"expiry": {"2h"}, "confirmed": {"1"}})
		if err != nil {
			t.Fatalf("POST /kill: %v", err)
		}
		_ = resp.Body.Close()
		// requireAuth's browser refusal is a 303 to /login (gate_test.go's
		// TestUnauthenticatedRedirects), never a 200 and never a redirect
		// anywhere the kill actually landed.
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unauthenticated kill status = %d, want 303 to /login", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("unauthenticated kill redirected to %q, want /login…", loc)
		}
		sils, err := h.store.ListSilences(ctx, true)
		if err != nil {
			t.Fatalf("ListSilences: %v", err)
		}
		if len(sils) != 0 {
			t.Fatalf("unauthenticated POST /kill created a silence: %+v", sils)
		}
	})

	t.Run("readonly scope is refused and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		token := h.mint(t, "viewer", "readonly")
		client := login(t, ts.URL, token)

		resp, err := client.PostForm(ts.URL+"/kill", url.Values{"expiry": {"2h"}, "confirmed": {"1"}})
		if err != nil {
			t.Fatalf("POST /kill: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("readonly kill status = %d, want 403", resp.StatusCode)
		}
		sils, err := h.store.ListSilences(ctx, true)
		if err != nil {
			t.Fatalf("ListSilences: %v", err)
		}
		if len(sils) != 0 {
			t.Fatalf("readonly POST /kill created a silence: %+v", sils)
		}
	})

	// The approve scope may settle a gated approval but must not ALSO carry
	// the write floor that fires the kill switch — the same disjointness the
	// machine API's canWrite/canApprove split enforces (AI-2's pattern).
	t.Run("approve scope is refused and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		token := h.mint(t, "approver", "approve")
		client := login(t, ts.URL, token)

		resp, err := client.PostForm(ts.URL+"/kill", url.Values{"expiry": {"2h"}, "confirmed": {"1"}})
		if err != nil {
			t.Fatalf("POST /kill: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("approve-scope kill status = %d, want 403", resp.StatusCode)
		}
		sils, err := h.store.ListSilences(ctx, true)
		if err != nil {
			t.Fatalf("ListSilences: %v", err)
		}
		if len(sils) != 0 {
			t.Fatalf("approve-scope POST /kill created a silence: %+v", sils)
		}
	})
}

// TestKillOperatorPausesAndBannerShows is the blue-phase proof: an operator
// token driving the hold-to-confirm form actually fires the global silence
// dashboard_data.go's banner() reads, and the very next dashboard render shows
// PAUSED — the same assertion banner_test.go makes for a CLI/store-created
// global silence, now reached through the web control.
func TestKillOperatorPausesAndBannerShows(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "operator", "operator")
	client := login(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/kill", url.Values{"expiry": {"2h"}, "confirmed": {"1"}})
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator kill status = %d, want 200 after redirect (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "PAUSED") {
		t.Errorf("post-kill page missing PAUSED banner:\n%s", bannerRegion(body))
	}

	sils, err := h.store.ListSilences(context.Background(), true)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(sils) != 1 || sils[0].ScopeKind != "global" {
		t.Fatalf("silences after kill = %+v, want exactly one global silence", sils)
	}
}

// TestKillRequiresHoldConfirmation proves the server re-asserts the
// hold-to-confirm gesture rather than trusting the client-rendered hidden
// field: a POST missing confirmed=1 is refused and mutates nothing, so a
// scripted or accidental submit of the bare form cannot fire the switch.
func TestKillRequiresHoldConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "operator", "operator")
	client := login(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/kill", url.Values{"expiry": {"2h"}})
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfirmed kill status = %d, want 400", resp.StatusCode)
	}
	sils, err := h.store.ListSilences(context.Background(), true)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(sils) != 0 {
		t.Fatalf("unconfirmed POST /kill created a silence: %+v", sils)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
