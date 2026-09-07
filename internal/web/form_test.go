package web

import (
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
// flash is success-only copy naming the netgroup and its range count.
func TestNetgroupSubmitAndFlash(t *testing.T) {
	assertClean(t, "submit", netgroupSubmitRefusal)
	if !strings.HasSuffix(netgroupSubmitRefusal, ".") {
		t.Errorf("submit refusal %q must end in a full stop", netgroupSubmitRefusal)
	}
	if got := netgroupAddedFlash("home", 2); got != "home added · 2 ranges" {
		t.Errorf("netgroupAddedFlash(home, 2) = %q", got)
	}
	if got := netgroupAddedFlash("home", 1); got != "home added · 1 range" {
		t.Errorf("netgroupAddedFlash(home, 1) = %q", got)
	}
}
