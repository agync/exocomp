package web

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// a11y_followups_test.go — the design-review accessibility follow-ups.
//  1. the launcher tablist is a tablist in name only: it answered click alone,
//     so a keyboard operator could reach the Command tab and never switch to
//     it (mouse-only control = a bug per INVARIANTS).
//  2. `.modal-close` is a 32px icon button, under the 48px phone floor.
//  3. the @toggle switch: does it need aria-checked? (verification, see
//     TestToggleSwitchNeedsNoAriaChecked.)

// launcherScript returns the launch drawer's inline tab script.
func launcherScript(page string) string {
	i := strings.Index(page, `#launch .tabs [role="tab"]`)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</script>")
	if j < 0 {
		return page[i:]
	}
	return page[i : i+j]
}

// tabSlot returns the rendered <button role="tab"> fragment for one tab.
func tabSlot(page, id string) string {
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(page[:i], "<button")
	j := strings.Index(page[i:], "</button>")
	if j < 0 {
		return page[i:]
	}
	return page[start : i+j]
}

// TestLauncherTabsRovingTabindexServerSide is half the keyboard gate: the
// roving tabindex is SERVER-rendered, so the JS-off baseline already carries
// one tab stop in the tablist (the selected tab) instead of two. The mutation
// (dropping tabindex, or rendering 0 on both) reds this.
func TestLauncherTabsRovingTabindexServerSide(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedGitSource(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name+"?launch=1")
	if code != 200 {
		t.Fatalf("/sources/%s?launch=1 = %d", name, code)
	}
	pb := tabSlot(page, "tab-playbook")
	cmd := tabSlot(page, "tab-command")
	if pb == "" || cmd == "" {
		t.Fatal("the launch tablist did not render both tabs")
	}
	if !strings.Contains(pb, `tabindex="0"`) {
		t.Errorf("the selected tab must carry the single tab stop: %s", pb)
	}
	if !strings.Contains(cmd, `tabindex="-1"`) {
		t.Errorf("the unselected tab must be out of the tab order: %s", cmd)
	}
	// ?tab=command flips the stop with the selection.
	_, cpage := getBody(t, client, ts.URL+"/sources/"+name+"?launch=1&tab=command")
	if !strings.Contains(tabSlot(cpage, "tab-command"), `tabindex="0"`) {
		t.Error("?tab=command must move the tab stop to the Command tab")
	}
	if !strings.Contains(tabSlot(cpage, "tab-playbook"), `tabindex="-1"`) {
		t.Error("?tab=command must take the Playbook tab out of the tab order")
	}
}

// TestLauncherTabsKeyboardMovesFocusAndSelects is the other half: the tablist
// handles keys, not clicks alone. ArrowLeft/ArrowRight step the selection with
// wrap, Home/End jump to the ends, and every move lands focus on the tab it
// activated. The mutation (deleting the keydown block) reds this.
func TestLauncherTabsKeyboardMovesFocusAndSelects(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedGitSource(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name+"?launch=1")
	if code != 200 {
		t.Fatalf("/sources/%s?launch=1 = %d", name, code)
	}
	script := launcherScript(page)
	if script == "" {
		t.Fatal("the launch tablist script is missing")
	}
	if !strings.Contains(script, `addEventListener("keydown"`) {
		t.Error("the launcher tabs must handle keydown (a click-only tablist is a mouse-only control)")
	}
	for _, key := range []string{"ArrowLeft", "ArrowRight", "Home", "End"} {
		if !strings.Contains(script, key) {
			t.Errorf("the launcher tabs must handle %s", key)
		}
	}
	// Wrap: stepping off either end comes back to the other tab, so the ring is
	// closed like a real tablist.
	if !strings.Contains(script, "tabs.length") {
		t.Error("arrow stepping must wrap using the tab count")
	}
	// Focus follows the selection, and the roving tabindex moves with it.
	if !strings.Contains(script, ".focus()") {
		t.Error("a keyboard move must land focus on the newly selected tab")
	}
	if !strings.Contains(script, `setAttribute("tabindex"`) {
		t.Error("select() must move the roving tabindex with aria-selected")
	}
	// The existing behaviour is untouched: click still selects, panels and the
	// drawer foot still toggle.
	for _, want := range []string{
		`addEventListener("click"`,
		`setAttribute("aria-selected"`,
		`panel.hidden = !on`,
		`f.hidden = f.getAttribute("data-foot") !== key`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the launcher script must keep %q", want)
		}
	}
}

// TestModalClosePhoneTouchFloor pins the a11y floor: on phone the modal and
// drawer close verb is at least 48px on both axes. Desktop stays 32px and the
// icon keeps its own size — the target grows, the glyph does not. The mutation
// (deleting the phone rule, or growing the base rule) reds this.
func TestModalClosePhoneTouchFloor(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	base := cssBlock(t, app, ".modal-close {")
	if !strings.Contains(base, "width: 32px") || !strings.Contains(base, "height: 32px") {
		t.Errorf("the desktop .modal-close must stay a 32px icon button: %s", base)
	}
	rule := phoneRuleBlock(t, app, ".modal-close {")
	if !strings.Contains(rule, "min-width: 48px") || !strings.Contains(rule, "min-height: 48px") {
		t.Errorf("the phone .modal-close must reach the 48px floor: %s", rule)
	}
	// The icon is not inflated with the target.
	if strings.Contains(rule, "svg") {
		t.Errorf("the phone rule must not resize the close icon: %s", rule)
	}
}

// phoneRuleBlock returns the declaration block for a selector inside the 719px
// phone media block, failing when the selector only exists outside one (a rule
// authored after the last phone block does not apply to phones).
func phoneRuleBlock(t *testing.T, css, open string) string {
	t.Helper()
	const at = "@media (max-width: 719px)"
	for i := 0; ; {
		j := strings.Index(css[i:], at)
		if j < 0 {
			break
		}
		i += j
		if k := mediaBlock(css[i:]); k != "" {
			if strings.Contains(k, open) {
				return cssBlock(t, k, open)
			}
		}
		i += len(at)
	}
	t.Fatalf("no 719px phone block carries the %q rule", strings.TrimSpace(open))
	return ""
}

// mediaBlock returns the brace-balanced body of the @media rule at the head of
// css, so a phone assertion reads only what the media query actually contains.
func mediaBlock(css string) string {
	open := strings.Index(css, "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[open : i+1]
			}
		}
	}
	return ""
}

// cssBlock returns the declaration block that opens at the given selector.
func cssBlock(t *testing.T, css, open string) string {
	t.Helper()
	i := strings.Index(css, open)
	if i < 0 {
		t.Fatalf("stylesheet lacks the %q rule", strings.TrimSpace(open))
	}
	j := strings.Index(css[i:], "}")
	if j < 0 {
		t.Fatalf("the %q rule is unclosed", strings.TrimSpace(open))
	}
	return css[i : i+j]
}

// TestToggleSwitchNeedsNoAriaChecked settles the open question about @toggle: a
// native checkbox with role="switch" already exposes its state, so aria-checked
// is redundant ARIA that a screen reader reads twice and can disagree with. The
// component therefore carries role="switch" and the label's association, and
// nothing else. The mutation (adding aria-checked, or dropping role="switch")
// reds this.
func TestToggleSwitchNeedsNoAriaChecked(t *testing.T) {
	var buf bytes.Buffer
	if err := toggle("privileged", "Run unsandboxed", true, false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render toggle: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `role="switch"`) {
		t.Errorf("the toggle must keep role=\"switch\": %s", html)
	}
	if strings.Contains(html, "aria-checked") {
		t.Errorf("aria-checked on a native checkbox is redundant; role=\"switch\" maps it already: %s", html)
	}
	var on bytes.Buffer
	if err := toggle("check", "check mode", true, false).Render(context.Background(), &on); err != nil {
		t.Fatalf("render toggle: %v", err)
	}
	if !strings.Contains(on.String(), "checked") {
		t.Errorf("the on state must stay the native checked attribute: %s", on.String())
	}
	var off bytes.Buffer
	if err := toggle("check", "check mode", false, false).Render(context.Background(), &off); err != nil {
		t.Fatalf("render toggle: %v", err)
	}
	if strings.Contains(off.String(), "checked") {
		t.Errorf("the off state must carry no checked attribute: %s", off.String())
	}
}
