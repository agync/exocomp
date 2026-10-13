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

// TestRowActionsDangerSeparator is the nit (4) gate: a destructive menu item
// is introduced by a VISUAL DIVIDER (a role=separator row) — never a stray
// "—" item — and a lone danger item (its own menu) draws no leading divider.
// The mutation (the divider dropped, or the stray dash back) reds here.
func TestRowActionsDangerSeparator(t *testing.T) {
	acts := []rowAction{
		{Name: "Open", Path: glyphOpen, Menu: true, Modal: "w-open"},
		{Name: "Remove", Path: glyphRemove, Menu: true, Form: "/w/p/rm", Danger: true},
	}
	var buf bytes.Buffer
	if err := rowActions(acts, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render rowActions: %v", err)
	}
	out := buf.String()
	div := strings.Index(out, `class="ramenu-sep" role="separator"`)
	if div < 0 {
		t.Error("the menu must draw a visual divider before the danger item")
	}
	if open := strings.Index(out, "> Open </button>"); open < 0 || div < open {
		t.Error("the divider must sit between the items and the danger item, never before the first")
	}
	// A lone danger item renders no leading divider.
	var lone bytes.Buffer
	if err := rowActions([]rowAction{{Name: "Remove", Path: glyphRemove, Menu: true, Form: "/w/p/rm", Danger: true}}, "").Render(context.Background(), &lone); err != nil {
		t.Fatalf("render lone: %v", err)
	}
	if strings.Contains(lone.String(), "ramenu-sep") {
		t.Error("a lone danger item must not draw a leading divider")
	}
	// No stray "—"-only menu item anywhere.
	if strings.Contains(out, `role="menuitem">—`) {
		t.Error("the menu must not render a stray em-dash item")
	}
}

// TestResourceRowActions pins the v0.6 row-action wiring for schedules, checks
// cards, and netgroups: each uses the rowActions partial (glyphs + menu), with
// the destructive Remove confirmed and last, and the per-resource verbs
// present.
func TestResourceRowActions(t *testing.T) {
	// Schedules: Run now + Disable glyphs + Open/Remove menu.
	sched := scheduleRowActions(scheduleRow{Name: "nightly", Enabled: true})
	if len(rowGlyphs(sched)) != 2 || rowGlyphs(sched)[0].Name != "Run now" || rowGlyphs(sched)[1].Name != "Disable" {
		t.Errorf("schedule glyphs = %+v, want [Run now, Disable]", rowGlyphs(sched))
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

// TestRowActionsRefusalRenders pins the row-verb refusal: a rowAction carrying
// a Refusal sentence renders the ▲ status line on the row (forms/SPEC.md) —
// never a flash (flash is success-only) — and an at-rest row renders none.
func TestRowActionsRefusalRenders(t *testing.T) {
	acts := []rowAction{
		{Name: "Remove", Path: glyphRemove, Form: "/ng/home/rm",
			Refusal: "Nothing was removed — confirm the removal in the dialog."},
	}
	var buf bytes.Buffer
	if err := rowActions(acts, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render rowActions: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`form-status is-error`,
		`role="alert"`,
		`Nothing was removed — confirm the removal in the dialog.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rowActions refusal missing %q", want)
		}
	}
	if strings.Contains(out, "flash") {
		t.Error("a row refusal must never render a flash")
	}

	var rest bytes.Buffer
	if err := rowActions([]rowAction{{Name: "Remove", Path: glyphRemove, Form: "/ng/home/rm"}}, "").Render(context.Background(), &rest); err != nil {
		t.Fatalf("render rowActions rest: %v", err)
	}
	if strings.Contains(rest.String(), "is-error") {
		t.Error("at-rest rowActions rendered a refusal line")
	}
}

// TestResourceRowRefusals pins the constructor wiring: each row verb's
// constructor carries the row's Refusal onto the row-actions so rowActions can
// render it — never the menu's link-only or modal-only actions.
func TestResourceRowRefusals(t *testing.T) {
	sched := scheduleRowActions(scheduleRow{Name: "nightly", Enabled: true, Refusal: "nope"})
	if rowActsRefusal(sched) != "nope" {
		t.Errorf("schedule row refusal lost: %q", rowActsRefusal(sched))
	}
	ck := checkRowActions(checkRow{Name: "presence", Enabled: true, Refusal: "nope"})
	if rowActsRefusal(ck) != "nope" {
		t.Errorf("check row refusal lost: %q", rowActsRefusal(ck))
	}
	ng := netgroupRowActions(netgroupRow{Name: "home", Refusal: "nope"})
	if rowActsRefusal(ng) != "nope" {
		t.Errorf("netgroup row refusal lost: %q", rowActsRefusal(ng))
	}
	sc := secretRowActions(secretRow{Name: "api-token", Refusal: "nope"}, true)
	if rowActsRefusal(sc) != "nope" {
		t.Errorf("secret row refusal lost: %q", rowActsRefusal(sc))
	}
	wsr := workspaceRowActions(workspaceWriteRow{Name: "web", TofuDir: "/srv/tofu/web", Refusal: "nope"})
	if rowActsRefusal(wsr) != "nope" {
		t.Errorf("workspace row refusal lost: %q", rowActsRefusal(wsr))
	}
	// A refused row never renders a flash in the row partial.
	var buf bytes.Buffer
	if err := rowActions(sched, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render rowActions: %v", err)
	}
	if strings.Contains(buf.String(), "flash") {
		t.Error("a row refusal must never render a flash")
	}
}
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
