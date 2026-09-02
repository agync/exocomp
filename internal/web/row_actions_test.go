package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
	if err := rowActions(acts).Render(context.Background(), &buf); err != nil {
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
