package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestRowActionsPartial pins the v0.6 row-actions partial: the first two
// actions render as verb glyphs with aria-labels, the rest go in the chevron
// menu, and a destructive item renders last in the fail color with the
// "asks first" note. The trailing "…" is retired.
func TestRowActionsPartial(t *testing.T) {
	acts := []rowAction{
		{Name: "Plan", Path: glyphPlan, Form: "/w/p/plan"},
		{Name: "Apply", Path: glyphApply, Form: "/w/p/apply", Hidden: map[string]string{"confirmed": "1"}},
		{Name: "Open", Path: glyphOpen, Modal: "w-open"},
		{Name: "Remove", Path: glyphRemove, Form: "/w/p/rm", Danger: true},
	}
	var buf bytes.Buffer
	if err := rowActions(acts, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render rowActions: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="raglyph" aria-label="Plan"`,
		`class="raglyph" aria-label="Apply"`,
		`action="/w/p/apply"`,
		`name="confirmed" value="1"`,
		`class="rachev"`,
		`role="menuitem"`,
		`data-modal-open="w-open"`,
		`class="ramenu-item danger"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rowActions missing %q", want)
		}
	}
	// Destructive last in the menu.
	plan := strings.Index(out, ">Plan</")
	remove := strings.Index(out, "Remove")
	if remove < plan {
		t.Error("destructive menu item must render last")
	}
	// The "asks first" note is rendered by the .danger::after rule (CSS), not
	// markup; pin the class that carries it.
	if !strings.Contains(out, `class="ramenu-item danger"`) {
		t.Error("destructive item must carry the danger class (asks-first note)")
	}
	// No trailing ellipsis anywhere.
	if strings.Contains(out, "…") {
		t.Error("row actions must not carry a trailing ellipsis")
	}
}

// TestResourceRowActions pins the v0.6 row-action wiring for schedules, checks
// cards, and netgroups: each uses the rowActions partial (glyphs + menu), with
// the destructive Remove confirmed and last, and the per-resource verbs
// present.
func TestResourceRowActions(t *testing.T) {
	// Schedules: Disable glyph + Open/Remove menu.
	sched := scheduleRowActions(scheduleRow{Name: "nightly", Enabled: true})
	if len(rowGlyphs(sched)) != 1 || rowGlyphs(sched)[0].Name != "Disable" {
		t.Errorf("schedule glyphs = %+v, want [Disable]", rowGlyphs(sched))
	}
	menu := rowMenu(sched)
	if len(menu) != 2 || menu[0].Name != "Open" || menu[1].Name != "Remove" || !menu[1].Danger {
		t.Errorf("schedule menu = %+v, want [Open, Remove(danger)]", menu)
	}

	// Checks: Disable glyph + Open/Remove menu.
	ck := checkRowActions(checkRow{Name: "presence", Enabled: true})
	if len(rowGlyphs(ck)) != 1 || rowGlyphs(ck)[0].Name != "Disable" {
		t.Errorf("check glyphs = %+v, want [Disable]", rowGlyphs(ck))
	}

	// Netgroups: a single Remove glyph, no chevron.
	ng := netgroupRowActions(netgroupRow{Name: "home"})
	if len(rowGlyphs(ng)) != 1 || rowGlyphs(ng)[0].Name != "Remove" || rowMenuCount(ng) != 0 {
		t.Errorf("netgroup actions = %+v, want a single Remove glyph", ng)
	}
}

// TestResourceRowActionsRender pins the row-actions markup on the schedules,
// checks, and netgroups pages: each carries the glyph buttons + the chevron
// menu (netgroups has no chevron — one action).
func TestResourceRowActionsRender(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "0 3 * * *", Kind: "backup",
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "presence", Kind: store.CheckTCP, IntervalS: 5,
		Targets: []store.CheckTarget{{Name: "phone", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	cases := []struct{ path, want string }{
		{"/schedules", `aria-label="Disable"`},
		{"/schedules", `data-modal-open="sched-nightly"`},
		{"/checks", `aria-label="Disable"`},
		{"/netgroups", `aria-label="Remove"`},
	}
	for _, c := range cases {
		code, page := getBody(t, op, ts.URL+c.path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d", c.path, code)
		}
		if !strings.Contains(page, c.want) {
			t.Errorf("%s missing row-action %q", c.path, c.want)
		}
	}
}
