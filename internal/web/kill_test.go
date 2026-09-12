package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestKillRefusesUnauthenticatedAndUnderScoped proves the approval page's
// "Silence & global kill" form (view_approval.templ, action "/kill") sits
// behind the operator floor: the control is destructive (it pauses the
// reactive engine, the dashboard's PAUSED state), so it must sit behind the
// SAME operator floor as the rest of the write surface (canWrite, api.go),
// not merely behind login. Both a caller with no session and a caller
// holding a readonly or approve token must be refused AND must leave no
// global silence behind — a mutation check reverts the scope check and
// watches this fail.
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
	// machine API's canWrite/canApprove split enforces.
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
// field: a POST missing confirmed=1 re-renders the approval surface at 422
// with the status line's refusal (the forms pattern) and mutates nothing, so a
// scripted or accidental submit of the bare form cannot fire the switch.
func TestKillRequiresHoldConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "operator", "operator")
	client := login(t, ts.URL, token)
	_, approval := h.gatedJob(t, time.Minute)

	resp, err := client.PostForm(ts.URL+"/kill",
		url.Values{"expiry": {"2h"}, "approval_id": {itoa64(approval.ID)}})
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed kill status = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `class="form-status is-error"`) ||
		!strings.Contains(body, "Hold the button to confirm the pause.") {
		t.Errorf("unconfirmed kill missing the status-line refusal:\n%s", body)
	}
	if !strings.Contains(body, `value="2h"`) {
		t.Errorf("unconfirmed kill lost the typed expiry value:\n%s", body)
	}
	sils, err := h.store.ListSilences(context.Background(), true)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(sils) != 0 {
		t.Fatalf("unconfirmed POST /kill created a silence: %+v", sils)
	}
}

// TestKillBadExpiry422KeepsValue pins the forms pattern on the kill form's
// expiry field: a non-duration expiry re-renders the approval surface at 422
// with the field's is-error meta line and the typed value kept — nothing is
// silenced.
func TestKillBadExpiry422KeepsValue(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "operator", "operator")
	client := login(t, ts.URL, token)
	_, approval := h.gatedJob(t, time.Minute)

	for _, expiry := range []string{"", "sometime", "-1h"} {
		resp, err := client.PostForm(ts.URL+"/kill",
			url.Values{"expiry": {expiry}, "confirmed": {"1"}, "approval_id": {itoa64(approval.ID)}})
		if err != nil {
			t.Fatalf("POST /kill: %v", err)
		}
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expiry=%q kill = %d, want 422 (%s)", expiry, resp.StatusCode, body)
		}
		if !strings.Contains(body, `id="kill-expiry-meta" class="meta is-error"`) ||
			!strings.Contains(body, "The expiry needs a positive duration") {
			t.Errorf("expiry=%q refusal missing the field's is-error meta:\n%s", expiry, body)
		}
		if !strings.Contains(body, `value="`+expiry+`"`) {
			t.Errorf("expiry=%q refusal lost the typed value:\n%s", expiry, body)
		}
	}
	sils, err := h.store.ListSilences(context.Background(), true)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(sils) != 0 {
		t.Fatalf("bad-expiry POST /kill created a silence: %+v", sils)
	}
}

// TestKillMetaAtRest pins the forms pattern on the kill form at rest: the
// expiry field's meta line renders its help text with no refusal, and the
// form carries the approval id that anchors its 422 re-render.
func TestKillMetaAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "operator", "operator")
	client := login(t, ts.URL, token)
	_, approval := h.gatedJob(t, time.Minute)

	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(approval.ID))
	if code != http.StatusOK {
		t.Fatalf("approval surface = %d, want 200", code)
	}
	for _, want := range []string{
		`id="kill-expiry-meta"`,
		"A silence always expires — like 2h or 45m.",
		`name="approval_id" value="` + itoa64(approval.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("kill form at rest missing %q", want)
		}
	}
	if strings.Contains(page, `class="meta is-error"`) {
		t.Error("kill form at rest must not carry a field refusal")
	}
}

// TestApprovalDecideSurfaceVerbs pins the button-verb anatomy on the decision
// forms: Reject and Approve render as their own forms posting decision values,
// a decided surface re-renders the outcome notice in place — the result lives
// on the surface, never a flash — and no decision forms remain.
func TestApprovalDecideSurfaceVerbs(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	_, approval := h.gatedJob(t, time.Minute)

	approve := h.mint(t, "human", "approve")
	client := login(t, ts.URL, approve)
	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(approval.ID))
	if code != http.StatusOK {
		t.Fatalf("approval surface = %d, want 200", code)
	}
	for _, want := range []string{
		`name="decision" value="reject"`,
		`name="decision" value="approve"`,
		`<button type="submit" class="btn danger">Reject</button>`,
		`<button type="submit" class="btn primary">Approve</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("decision surface missing %q", want)
		}
	}

	resp, err := client.PostForm(ts.URL+"/approvals/"+itoa64(approval.ID)+"/decide",
		url.Values{"decision": {"reject"}})
	if err != nil {
		t.Fatalf("POST decide: %v", err)
	}
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reject status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Recorded: rejected") || !strings.Contains(body, "was cancelled") {
		t.Errorf("decided surface missing the outcome notice:\n%s", body)
	}
	if strings.Contains(body, `name="decision"`) {
		t.Error("a decided surface must not re-render the decision forms")
	}
	settled, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State != store.ApprovalRejected {
		t.Fatalf("approval state = %q, want rejected", settled.State)
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
