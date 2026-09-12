package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
)

// seedSystemShell sets the one /system setting the identity strip's right
// cluster renders: the public URL value beside its Change verb.
func seedSystemShell(t *testing.T, h *harness) {
	t.Helper()
	if err := h.store.PutSetting(context.Background(), store.SettingPublicURL,
		"https://exocomp.example.ts.net"); err != nil {
		t.Fatalf("PutSetting public url: %v", err)
	}
}

// TestSystemShellIdentity is the GATE for the §0.1 identity strip: the build
// stamp (exocomp {version} · commit · platform) and the host fact (on {host} ·
// up {uptime}) render on the left, and the ONE existing verb — public URL
// value + Change, posting to the landed L-B3 write — sits on the right. Doctor
// and Recovery kit render as read-only header glyphs until L-S5 lands their
// verbs.
func TestSystemShellIdentity(t *testing.T) {
	h := newHarness(t)
	seedSystemShell(t, h)
	version.Version, version.Commit = "0.12.0", "e741e71"
	defer func() { version.Version, version.Commit = "dev", "dev" }()
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`class="sys-identity"`,
		"exocomp 0.12.0",
		"e741e71 · " + runtime.GOOS + "/" + runtime.GOARCH,
		`<span class="host">` + hostname() + `</span>`,
		" · up ",
		"is the control plane itself OK",
		// The header glyphs are live since L-S5 (A1/A3): each is a POST verb
		// with the meta beside it, no longer read-only.
		`title="Doctor — run the self-check"`,
		`action="/system/doctor"`,
		`title="Recovery kit — reprint · read-only: exocomp kit"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("identity strip missing %q", want)
		}
	}
	// The one verb: value link + Change posting to the landed write.
	for _, want := range []string{
		"public URL",
		"https://exocomp.example.ts.net",
		`action="/system/public-url"`,
		`name="url"`,
		">Change</button>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("public-URL cluster missing %q", want)
		}
	}
}

// TestSystemShellLegGrid is the GATE for the §0.1 2×2 leg grid: four leg cards
// with the shared anatomy (word · figure · facts · one verb) render as stubs
// under the ids the powerline anchors land on. The leg lanes fill the card
// bodies next.
func TestSystemShellLegGrid(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `class="sys-legs"`) {
		t.Error("missing the 2x2 leg grid")
	}
	for _, leg := range []struct {
		id, name string
	}{
		{id: "deadman", name: "Dead-man"},
		{id: "ntfy", name: "ntfy"},
		{id: "litestream", name: "Litestream"},
		{id: "backups", name: "Backups"},
	} {
		if !strings.Contains(page, `<section class="leg" id="`+leg.id+`"`) {
			t.Errorf("missing the %s leg card", leg.id)
		}
		if !strings.Contains(page, ">"+leg.name+"</span>") {
			t.Errorf("leg %s missing its word %q", leg.id, leg.name)
		}
	}
	// The §0.1 anatomy slots every leg card carries.
	for _, slot := range []string{
		`class="leg-head"`, `class="verbs"`, `class="leg-figure"`,
		`class="leg-facts"`, `class="leg-sentence"`,
	} {
		if !strings.Contains(page, slot) {
			t.Errorf("leg anatomy missing %q", slot)
		}
	}
}

// TestSystemShellPowerline is the GATE for the §0.1 powerline: the version
// lead plus four per-segment anchors, each onto its leg card. Dropping any one
// anchor must red (mutation).
func TestSystemShellPowerline(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `class="sys-powerline"`) {
		t.Error("missing the self-care powerline")
	}
	for _, want := range []string{
		`href="/system#deadman"`,
		`href="/system#ntfy"`,
		`href="/system#litestream"`,
		`href="/system#backups"`,
		"dead-man ",
		"ntfy ",
		"rpo ",
		"backups ",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("powerline missing segment %q", want)
		}
	}
}

// TestSystemShellPhoneRows is the GATE for the §0.2 phone collapse: at ≤720
// the cards give way to two-line rows, each a link to its leg's own page
// (404 until the leg lanes land — acceptable, the links are the contract).
func TestSystemShellPhoneRows(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `class="sys-phone"`) {
		t.Error("missing the phone collapse container")
	}
	for _, want := range []string{
		`href="/system/deadman"`,
		`href="/system/ntfy"`,
		`href="/system/litestream"`,
		`href="/system/backups"`,
		`class="leg-row"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("phone rows missing %q", want)
		}
	}
}
