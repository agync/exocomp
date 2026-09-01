package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestNetgroupsSurfaceGated is the gate for the netgroups view: it is
// token-gated (no session refused, session renders), it shows every group's
// prefixes in full, the nav carries Netgroups, and an operator sees the
// add/rm write controls while a readonly session sees none.
func TestNetgroupsSurfaceGated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	if _, err := h.store.CreateNetgroup(context.Background(), store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	// No session: refused with a redirect to the login form.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/netgroups")
	if err != nil {
		t.Fatalf("GET /netgroups no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/netgroups without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/netgroups redirect = %q, want /login…", loc)
	}

	opClient := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// The standalone /netgroups page is a merge redirect (F7): it points at the
	// Netgroups tab on /checks.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	noFollow.Jar = opClient.Jar
	resp, err = noFollow.Get(ts.URL + "/netgroups")
	if err != nil {
		t.Fatalf("GET /netgroups as operator: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/netgroups as operator = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/checks?tab=netgroups" {
		t.Fatalf("/netgroups redirect = %q, want /checks?tab=netgroups", loc)
	}

	// Operator: the Netgroups tab on /checks renders the group and controls.
	code, page := getBody(t, opClient, ts.URL+"/checks?tab=netgroups")
	if code != http.StatusOK {
		t.Fatalf("/checks?tab=netgroups with session = %d, want 200", code)
	}
	for _, want := range []string{
		`scope="col"`, "home", "192.168.0.0/16",
		`action="/netgroups/add"`, `/netgroups/home/rm`,
		`href="/checks" aria-current="page"`,
		`href="/checks?tab=netgroups" role="tab" aria-selected="true"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks?tab=netgroups (operator) missing %q", want)
		}
	}

	// Readonly: the tab renders, without any write control.
	ro := h.mint(t, "ro", "readonly")
	roClient := login(t, ts.URL, ro)
	code, page = getBody(t, roClient, ts.URL+"/checks?tab=netgroups")
	if code != http.StatusOK {
		t.Fatalf("/checks?tab=netgroups readonly = %d, want 200", code)
	}
	if !strings.Contains(page, "Read-only: managing netgroups needs an operator token.") {
		t.Error("/checks?tab=netgroups (readonly) missing the read-only note")
	}
	for _, banned := range []string{`action="/netgroups/add"`, `/rm"`} {
		if strings.Contains(page, banned) {
			t.Errorf("/checks?tab=netgroups (readonly) leaked write control %q", banned)
		}
	}
}

// TestNetgroupsReachableFromChecks: the Netgroups surface is a tab on /checks
// (F7), so the checks page carries the tab link and the nav points at /checks.
func TestNetgroupsReachableFromChecks(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/checks")
	if !strings.Contains(page, `href="/checks?tab=netgroups" role="tab"`) {
		t.Error("checks page missing the Netgroups tab link (F7)")
	}
	if strings.Contains(page, `href="/netgroups"`) {
		t.Error("nav still carries a standalone Netgroups item (F7)")
	}
}

func TestNetgroupAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"home"}, "prefixes": {"192.168.0.0/16,10.0.0.0/8"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("netgroup add = %d, want 303", resp.StatusCode)
	}

	g, err := h.store.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("GetNetgroup: %v", err)
	}
	if len(g.Prefixes) != 2 {
		t.Fatalf("prefixes = %d, want 2", len(g.Prefixes))
	}
}

// TestNetgroupAddValidatesPrefixes: a CIDR that does not parse is refused
// before the store sees it (the same store.ParsePrefixList `netgroup add` runs),
// the rejection resolves as a flash-carrying redirect, and no row lands.
func TestNetgroupAddValidatesPrefixes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"bad"}, "prefixes": {"not-a-cidr"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "not-a-cidr") {
		t.Errorf("flash message %q does not name the rejected CIDR", msg)
	}
	if _, err := h.store.GetNetgroup(context.Background(), "bad"); err == nil {
		t.Fatal("a netgroup was created despite an invalid CIDR")
	}
}

// TestNetgroupAddDuplicateFlashes: re-adding a name that exists is refused
// with a flash-carrying redirect, not a 409, and the original row survives.
func TestNetgroupAddDuplicateFlashes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"home"}, "prefixes": {"192.168.0.0/16"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "already exists") {
		t.Errorf("flash message %q does not report the duplicate", msg)
	}
	if _, err := h.store.GetNetgroup(ctx, "home"); err != nil {
		t.Fatalf("original netgroup vanished: %v", err)
	}
}

func TestNetgroupAddReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, ro, ts.URL+"/netgroups/add",
		url.Values{"name": {"home"}, "prefixes": {"192.168.0.0/16"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly netgroup add = %d, want 403", resp.StatusCode)
	}
}

// TestNetgroupAddBadNameFlashes: a name the store's ValidName gate refuses
// is operator input, not a store failure: it must come back as a flash-
// carrying redirect, never a bare 500, and no row lands.
func TestNetgroupAddBadNameFlashes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"bad name!"}, "prefixes": {"192.168.0.0/16"}}, nil)
	msg := flashMessage(t, resp, "/checks")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "must match") {
		t.Errorf("flash message %q does not report the name refusal", msg)
	}
	if _, err := h.store.GetNetgroup(ctx, "bad name!"); err == nil {
		t.Fatal("a netgroup was created despite an invalid name")
	}
}

func TestNetgroupRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/netgroups/home/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetNetgroup(ctx, "home"); err == nil {
		t.Fatal("netgroup still present after rm")
	}
}

// TestAPINetgroupsList: a readonly token reads the netgroup.list projection
// with the seeded group's prefixes, the same bytes `netgroup list --output
// json` emits.
func TestAPINetgroupsList(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	if _, err := h.store.CreateNetgroup(context.Background(), store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	code, _, body := bearerGet(t, ts.URL, "/api/v1/netgroups", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/netgroups status = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"kind":"`+wire.KindNetgroupList+`"`) {
		t.Fatalf("netgroups kind missing: %s", body)
	}
	if !strings.Contains(body, `"name":"home"`) || !strings.Contains(body, `"prefixes":["192.168.0.0/16"]`) {
		t.Fatalf("netgroup row not projected: %s", body)
	}
}

// TestNetgroupsLinkToEvents: a netgroup name links to the events the check
// sweep emits for the addresses it classifies.
func TestNetgroupsLinkToEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/netgroups")
	if code != 200 {
		t.Fatalf("/netgroups status = %d", code)
	}
	if !strings.Contains(page, `<a href="/events?source=check">home</a>`) {
		t.Error("/netgroups missing the linked netgroup name")
	}
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}
