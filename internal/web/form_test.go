package web

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// banned are the design's never words (forms/SPEC.md "Words"): a refusal
// sentence must not leak the validator's vocabulary, a constraint name, or the
// meta-words the copy table forbids. The translations are the one place they
// must never appear.
var banned = []string{
	"invalid", "error", "oops", "please", "sorry",
	"must match", "is not a CIDR", "ErrValidation", "prefix %d",
}

// assertClean asserts the copy translations carry none of the banned words, so
// a regression that starts surfacing the raw validator text fails here.
func assertClean(t *testing.T, kind string, parts ...string) {
	t.Helper()
	for _, s := range parts {
		lower := strings.ToLower(s)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("%s %q contains banned word %q", kind, s, word)
			}
		}
	}
}

// TestNetgroupNameRefusal pins the name field's translations (forms/copy.txt):
// empty names get the required sentence, spaced names the spaces sentence plus
// a safe slug, and other charset violations the charset sentence. The fix is
// offered only when the slug is non-empty and differs.
func TestNetgroupNameRefusal(t *testing.T) {
	cases := []struct {
		name, wantMsg, wantFix string
	}{
		{"", "Give the netgroup a name.", ""},
		{"   ", "Give the netgroup a name.", ""},
		{"Home CIDR", "Names can't have spaces.", "home-cidr"},
		{"Home  CIDR", "Names can't have spaces.", "home-cidr"},
		{"home!", "Names use letters, digits, dots, hyphens and underscores.", "home"},
		{"a/b", "Names use letters, digits, dots, hyphens and underscores.", "a-b"},
		{"---", "Names use letters, digits, dots, hyphens and underscores.", ""},
		{"OK_name.v1", "", ""},
	}
	for _, tc := range cases {
		if store.ValidName(tc.name) {
			continue // a valid name is never refused; only the refusal cases matter
		}
		msg, fix := netgroupNameRefusal(tc.name)
		if msg != tc.wantMsg {
			t.Errorf("netgroupNameRefusal(%q) msg = %q, want %q", tc.name, msg, tc.wantMsg)
		}
		if fix != tc.wantFix {
			t.Errorf("netgroupNameRefusal(%q) fix = %q, want %q", tc.name, fix, tc.wantFix)
		}
		assertClean(t, "name", msg, fix)
	}
}

// TestNetgroupSlug pins the slug rule (forms/copy.txt): lowercase, trim,
// collapse bad runs to a dash, strip edge dashes.
func TestNetgroupSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Home CIDR", "home-cidr"},
		{"  home!", "home"},
		{"a!!b", "a-b"},
		{"a--!-b", "a----b"},
		{"-home-", "home"},
		{"", ""},
		{"---", ""},
		{"ok_name.v1", ""}, // equals the input: not offered
	}
	for _, tc := range cases {
		if got := netgroupSlug(tc.in); got != tc.want {
			t.Errorf("netgroupSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNetgroupCIDRRefusal pins the CIDR field's translations against the real
// ParsePrefixList errors (forms/copy.txt): each refusal names the offending
// member and says in the field's terms what is wrong. An empty list reports
// the required sentence with no item.
func TestNetgroupCIDRRefusal(t *testing.T) {
	cases := []struct {
		field    string
		wantItem string
		wantMsg  string
	}{
		{"", "", "Add at least one range, like 192.168.0.0/16."},
		{"   ", "", "Add at least one range, like 192.168.0.0/16."},
		{"192.168.0.0/16,,10.0.0.0/8", "", "Add at least one range, like 192.168.0.0/16."},
		{"192.168.0/16", "192.168.0/16", " needs four octets — like 192.168.0.0/16."},
		{"1.2.3.4.5/16", "1.2.3.4.5/16", " needs four octets — like 192.168.0.0/16."},
		{"192.168.0.0", "192.168.0.0", " has no prefix length — try 192.168.0.0/24 or 192.168.0.0/8."},
		{"10.0.0.0/33", "10.0.0.0/33", " has a prefix over /32."},
		{"fd00::/130", "fd00::/130", " has a prefix over /128."},
		{"fd00:g::1/8", "fd00:g::1/8", " isn't an IPv6 range — like fd00::/8."},
	}
	for _, tc := range cases {
		_, err := store.ParsePrefixList(tc.field)
		if err == nil {
			t.Fatalf("%q: expected a parse refusal", tc.field)
		}
		item, msg := netgroupCIDRRefusal(tc.field, err)
		if item != tc.wantItem {
			t.Errorf("netgroupCIDRRefusal(%q) item = %q, want %q", tc.field, item, tc.wantItem)
		}
		if msg != tc.wantMsg {
			t.Errorf("netgroupCIDRRefusal(%q) msg = %q, want %q", tc.field, msg, tc.wantMsg)
		}
		assertClean(t, "cidr", item, msg)
	}
}

// TestNetgroupSubmitAndFlash pin the status line and the success flash: the
// status line is a whole-submit refusal that names the data's fate, and the
// flash is success-only copy naming the netgroup and its range count (ASCII,
// since the flash cookie sanitizes non-ASCII bytes to spaces).
func TestNetgroupSubmitAndFlash(t *testing.T) {
	assertClean(t, "submit", netgroupSubmitRefusal)
	if !strings.HasSuffix(netgroupSubmitRefusal, ".") {
		t.Errorf("submit refusal %q must end in a full stop", netgroupSubmitRefusal)
	}
	if got := netgroupAddedFlash("home", 2); got != "home added (2 ranges)" {
		t.Errorf("netgroupAddedFlash(home, 2) = %q", got)
	}
	if got := netgroupAddedFlash("home", 1); got != "home added (1 range)" {
		t.Errorf("netgroupAddedFlash(home, 1) = %q", got)
	}
}

// TestFormStatusPendingLabel pins the parametrized submitting label: formStatus
// renders the verb the caller names ("Adding check", "Adding schedule") while
// submitting, and the two-argument call — what the netgroup/source exemplars
// still make — keeps the "Adding" default, so a converted form's pending state
// reads in its own verb without the primitive knowing the form and without
// breaking an already-landed exemplar.
func TestFormStatusPendingLabel(t *testing.T) {
	cases := []struct{ label, want string }{
		{"", "Adding"}, // the exemplars' two-argument call
		{"Adding check", "Adding check"},
		{"Adding schedule", "Adding schedule"},
		{"Adding policy", "Adding policy"},
		{"Adding runner", "Adding runner"},
	}
	for _, tc := range cases {
		if got := renderFormStatus(t, true, "", tc.label); !strings.Contains(got, tc.want) {
			t.Errorf("formStatus label %q missing %q\ngot: %s", tc.label, tc.want, got)
		}
	}
	// At rest — neither submitting nor refused — the line carries no label.
	if got := renderFormStatus(t, false, "", "Adding check"); strings.Contains(got, "Adding check") {
		t.Errorf("pending label rendered at rest:\n%s", got)
	}
	// A refusal renders the ▲ sentence with Retry; the label stays out of it.
	if got := renderFormStatus(t, true, "Couldn't reach exocompd — nothing was saved.", "Adding check"); !strings.Contains(got, "Retry") {
		t.Errorf("refusal missing Retry:\n%s", got)
	}
}

// renderFormStatus renders the shared status line standalone, the way any
// surface's @formStatus(...) call composes it in a .templ file. An empty label
// is the two-argument call's default.
func renderFormStatus(t *testing.T, pending bool, msg, label string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := formStatus(pending, msg, label).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render formStatus: %v", err)
	}
	return buf.String()
}
