package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
		"at-home", "tcp", "5m0s",
		"laptop", "nas", "192.168.1.50:22",
		`<span class="chip ok"><span class="dot"></span>UP</span>`,
		`<span class="chip fail"><span class="dot"></span>DOWN</span>`,
		`action="/checks/add"`, `/checks/at-home/disable`, `/checks/at-home/rm`,
		`href="/checks" aria-current="page"`,
		"nothing fires until a rule matches", `href="/rules">/rules</a>`,
		`class="checkcard"`, `class="targetgrid"`,
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

// flashMessage extracts the ?flash= message a rejected form redirect carries,
// asserting the redirect went back to wantPath. The message is the operator's
// one-line error; a caller asserts a stable substring of it.
func flashMessage(t *testing.T, resp *http.Response, wantPath string) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, wantPath+"?flash=") {
		t.Fatalf("redirect = %q, want %s?flash=…", loc, wantPath)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(loc, wantPath+"?"))
	if err != nil {
		t.Fatalf("parse redirect query %q: %v", loc, err)
	}
	if msg := q.Get("flash"); msg != "" {
		return msg
	}
	t.Fatalf("redirect %q carries no flash message", loc)
	return ""
}

// TestCheckAddValidatesTargets: a tcp target without its ip:port shape is
// refused before the store sees it (the same parseCheckTargets `check add`
// runs), the rejection resolves as a flash-carrying redirect, and no row
// lands.
func TestCheckAddValidatesTargets(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad"}, "kind": {store.CheckTCP}, "interval": {"5m"}, "target": {"laptop"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "laptop") {
		t.Errorf("flash message %q does not name the rejected target", msg)
	}
	if _, err := h.store.GetCheck(context.Background(), "bad"); err == nil {
		t.Fatal("a check was created despite an invalid target")
	}
}

// TestCheckAddBadNameFlashes: a name the store's ValidName gate refuses is
// operator input, not a store failure: it must come back as a flash-carrying
// redirect, never a bare 500, and no row lands.
func TestCheckAddBadNameFlashes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad name!"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "must match") {
		t.Errorf("flash message %q does not report the name refusal", msg)
	}
	if _, err := h.store.GetCheck(context.Background(), "bad name!"); err == nil {
		t.Fatal("a check was created despite an invalid name")
	}
}

// TestCheckAddDuplicateFlashes: re-adding a name that exists is refused with
// a flash-carrying redirect, not a 409, and the original row survives.
func TestCheckAddDuplicateFlashes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"at-home"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "already exists") {
		t.Errorf("flash message %q does not report the duplicate", msg)
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err != nil {
		t.Fatalf("original check vanished: %v", err)
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

// TestChecksNetgroupAndRecency: location is prose, not an equal-rank chip (F2)
// — a target a registered netgroup covers reads "address is on <group>", one
// no group covers reads the away miss, an http URL target renders no location
// verdict at all (its liveness is not a location). A target the sweep has seen
// renders how long ago its status changed, right-aligned in its tile header
// (F13); a never-seen target is a single dim UNOBSERVED chip with its
// cold-start reason and no location chip (F3).
func TestChecksNetgroupAndRecency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	// tcp targets: laptop inside "home", nas outside every group.
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "10.0.0.5", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("seed tcp check: %v", err)
	}
	// An http target: a URL, no address, so no netgroup classification.
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "web-health", Kind: store.CheckHTTP, IntervalS: 60,
		Targets: []store.CheckTarget{{Name: "web", URL: "https://web.lan/health"}},
	}); err != nil {
		t.Fatalf("seed http check: %v", err)
	}

	// A third target the sweep has never seen: the cold-start unobserved tile.
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "cold", Kind: store.CheckPing, IntervalS: 15,
		Targets: []store.CheckTarget{{Name: "tablet", Address: "192.168.1.60"}},
	}); err != nil {
		t.Fatalf("seed cold check: %v", err)
	}

	// Seed states with LastChange comfortably inside one humanAge bucket: the
	// render happens milliseconds after seeding, so a boundary-seeded offset
	// could truncate one step down before it reaches the page.
	now := time.Now().UTC()
	for _, st := range []store.CheckState{
		{CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
			Consecutive: 3, Emitted: store.CheckUp,
			FirstSeen: now.Add(-24 * time.Hour), LastChange: now.Add(-(3*time.Hour + 15*time.Minute))},
		{CheckID: c.ID, TargetName: "nas", Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown,
			FirstSeen: now.Add(-24 * time.Hour), LastChange: now.Add(-(2*time.Hour + 15*time.Minute))},
	} {
		if err := h.store.SetCheckState(ctx, st); err != nil {
			t.Fatalf("SetCheckState %s: %v", st.TargetName, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}

	// Location is prose, not an equal-rank chip (F2): the covered target names
	// its group, the uncovered one names the miss. The per-target clock sits
	// right-aligned in the tile header (F13) and the header carries the
	// at-a-glance down counter.
	for _, want := range []string{
		`address is on <b>home</b>`,
		`address not on any netgroup (away)`,
		`3h ago`, `2h ago`,
		`1 of 2 down`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing %q", want)
		}
	}
	// Reachability and location are never two equal chips on the same tile: the
	// netgroup is prose, so no home/away chip survives the rebuild.
	if strings.Contains(page, `>home</span>`) || strings.Contains(page, `>away</span>`) {
		t.Error("a netgroup still renders as a chip on the checks surface (F2)")
	}

	// The http target renders no location verdict at all: cut its card and
	// check for any location prose.
	seg := page[strings.Index(page, "web-health"):]
	if strings.Contains(seg, "address is on") || strings.Contains(seg, "not on any netgroup") {
		t.Error("http target rendered a location verdict")
	}

	// The never-seen tile is a single dim UNOBSERVED chip with its cold-start
	// reason, never a location chip (F3).
	cold := page[strings.Index(page, "cold"):]
	for _, want := range []string{
		`<span class="chip dim"><span class="dot"></span>UNOBSERVED</span>`,
		"never seen yet — first result decides",
	} {
		if !strings.Contains(cold, want) {
			t.Errorf("cold-start tile missing %q", want)
		}
	}
	if strings.Contains(cold, "address is on") || strings.Contains(cold, "not on any netgroup") {
		t.Error("cold-start tile paired UNOBSERVED with a location chip (F3)")
	}
}

// TestChecksLinkToEvents: the check name links to the check source's events,
// so an operator reaches what a check raised. The netgroup is prose now (F2),
// not a chip link.
func TestChecksLinkToEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/checks")
	if code != 200 {
		t.Fatalf("/checks status = %d", code)
	}
	for _, want := range []string{
		`<a class="mono" href="/events?source=check">at-home</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing %q", want)
		}
	}
	if strings.Contains(page, `/events?source=check"><span class="chip`) {
		t.Error("a netgroup chip still links to events; location is prose (F2)")
	}
}

// TestCheckAddTargetControl:the check-add modal carries an Add target control
// that clones the last target input, plus the vanilla JS that wires it — the four
// baseline inputs staying the JS-off baseline.
func TestCheckAddTargetControl(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks status = %d, want 200", code)
	}
	for _, want := range []string{
		`id="ck-add-target"`,
		`>Add target</button>`,
		`cloneNode`,
		`inputs.length >= 10`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing the add-target control %q", want)
		}
	}
}

// TestListEmptyStatesNameNextAction: every registered-resource surface names
// its next action when nothing is registered yet, so a cold start reads as a
// next step instead of a dead end (the brief's empty states must not name
// the tool, but the action).
func TestListEmptyStatesNameNextAction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct {
		path, want string
	}{
		{"/checks", "No checks registered — add one."},
		{"/netgroups", "No netgroups registered — add one."},
		{"/rules", "No rules registered — rules are written on the CLI."},
		{"/runners", "No runners registered — add one."},
		{"/schedules", "No schedules registered — add one."},
		{"/sources", "No sources registered — add one."},
		{"/workspaces", "No state registered — add a workspace."},
	} {
		code, page := getBody(t, op, ts.URL+tc.path)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, code)
		}
		if !strings.Contains(page, tc.want) {
			t.Errorf("%s empty state missing %q", tc.path, tc.want)
		}
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
