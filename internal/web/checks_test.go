package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// seedTCPCheck registers a tcp check with two targets and writes each one's
// state row — laptop up, nas down — so the page can render both statuses.
func seedTCPCheck(t *testing.T, h *harness) store.Check {
	t.Helper()
	ctx := context.Background()
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "192.168.1.60", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		target, status string
	}{
		{"laptop", store.CheckUp},
		{"nas", store.CheckDown},
	} {
		if err := h.store.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: tc.target, Status: tc.status,
			Consecutive: 3, Emitted: tc.status, FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("SetCheckState %s: %v", tc.target, err)
		}
	}
	return c
}

// TestChecksSurfaceGated is the gate for the checks view: it is token-gated
// (no session refused, session renders), it shows the check with each
// target's last observed status, the nav carries Checks, and an operator sees
// the add/enable/disable/rm write controls while a readonly session sees
// none.
func TestChecksSurfaceGated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)

	// No session: refused with a redirect to the login form.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/checks")
	if err != nil {
		t.Fatalf("GET /checks no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/checks without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/checks redirect = %q, want /login…", loc)
	}

	// Operator: the surface renders with the check, per-target statuses, and
	// the write controls.
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks with session = %d, want 200", code)
	}
	for _, want := range []string{
		`scope="col"`, "All checks", "at-home", "tcp", "5m0s",
		"laptop", "nas", "192.168.1.50:22",
		`<span class="chip ok"><span class="dot"></span>UP</span>`,
		`<span class="chip fail"><span class="dot"></span>DOWN</span>`,
		`action="/checks/add"`, `/checks/at-home/disable`, `/checks/at-home/rm`,
		`href="/checks" aria-current="page"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks (operator) missing %q", want)
		}
	}

	// Readonly: the page renders, without any write control.
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	code, page = getBody(t, ro, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks readonly = %d, want 200", code)
	}
	if !strings.Contains(page, "Read-only: managing checks needs an operator token.") {
		t.Error("/checks (readonly) missing the read-only note")
	}
	for _, banned := range []string{`action="/checks/add"`, `/checks/at-home/disable`, `/checks/at-home/rm`} {
		if strings.Contains(page, banned) {
			t.Errorf("/checks (readonly) leaked write control %q", banned)
		}
	}
}

// TestChecksNavElsewhere: the Checks nav item appears on other pages too, so
// it is reachable from anywhere in the surface.
func TestChecksNavElsewhere(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	if !strings.Contains(page, `href="/checks"`) || !strings.Contains(page, ">Checks</span>") {
		t.Error("dashboard nav missing the Checks link")
	}
}

// TestCheckAddCreates drives the add form end to end: repeated target fields
// parse into two tcp targets and the row lands enabled. Kind tcp dials only
// on the sweep, so no live network is touched at create time (unlike ping).
func TestCheckAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name":     {"at-home"},
			"kind":     {store.CheckTCP},
			"interval": {"5m"},
			"target":   {"laptop=192.168.1.50:22", "nas=192.168.1.60:22"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check add = %d, want 303", resp.StatusCode)
	}

	c, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if c.Kind != store.CheckTCP {
		t.Fatalf("kind = %q, want tcp", c.Kind)
	}
	if len(c.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(c.Targets))
	}
	if !c.Enabled {
		t.Fatal("newly created check should be enabled")
	}
}

// TestCheckAddHTTPExpect drives the http add path with a pinned expected
// status, so the expect field lands on every target.
func TestCheckAddHTTPExpect(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name":     {"web-health"},
			"kind":     {store.CheckHTTP},
			"interval": {"1m"},
			"target":   {"web=https://web.lan/health"},
			"expect":   {"200"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("http check add = %d, want 303", resp.StatusCode)
	}

	c, err := h.store.GetCheck(ctx, "web-health")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if len(c.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(c.Targets))
	}
	if c.Targets[0].URL != "https://web.lan/health" {
		t.Fatalf("target url = %q, want https://web.lan/health", c.Targets[0].URL)
	}
	if c.Targets[0].Expect != 200 {
		t.Fatalf("target expect = %d, want 200", c.Targets[0].Expect)
	}
}

// TestCheckAddValidatesTargets: a tcp target without its ip:port shape is
// refused before the store sees it (the same parseCheckTargets `check add`
// runs), and no row lands.
func TestCheckAddValidatesTargets(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad"}, "kind": {store.CheckTCP}, "interval": {"5m"}, "target": {"laptop"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a bare name= target without a port was accepted")
	}
	if _, err := h.store.GetCheck(context.Background(), "bad"); err == nil {
		t.Fatal("a check was created despite an invalid target")
	}
}

func TestCheckAddReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, ro, ts.URL+"/checks/add",
		url.Values{"name": {"at-home"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly check add = %d, want 403", resp.StatusCode)
	}
}

func TestCheckEnableDisable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/disable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable = %d, want 303", resp.StatusCode)
	}
	c, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if c.Enabled {
		t.Fatal("check still enabled after disable")
	}

	resp = postForm(t, op, ts.URL+"/checks/at-home/enable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable = %d, want 303", resp.StatusCode)
	}
	c, err = h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !c.Enabled {
		t.Fatal("check still disabled after enable")
	}
}

func TestCheckRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err == nil {
		t.Fatal("check still present after rm")
	}
}

// TestAPIChecksList: a readonly token reads the check.list projection with
// the seeded check and its target, the same bytes `check list --output json`
// emits.
func TestAPIChecksList(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	if _, err := h.store.CreateCheck(context.Background(), store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	code, _, body := bearerGet(t, ts.URL, "/api/v1/checks", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/checks status = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"kind":"`+wire.KindCheckList+`"`) {
		t.Fatalf("checks kind missing: %s", body)
	}
	if !strings.Contains(body, `"name":"at-home"`) || !strings.Contains(body, `"address":"192.168.1.50"`) {
		t.Fatalf("check row not projected: %s", body)
	}
}
