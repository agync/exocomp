package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// today is the date the reprint verb stamps, the same clock the kit meta
// renders.
func today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// TestKitReprintStampsAndResets is the GATE for A3's reprint verb: the
// Recovery-kit glyph posts /system/kit/reprint, which stamps the printed date
// and resets the staleness counter, and the identity strip then reads
// `kit printed {date} · 0 changes since`.
func TestKitReprintStampsAndResets(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/system/kit/reprint", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reprint status = %d, want 303", resp.StatusCode)
	}

	code, page := getBody(t, op, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, "kit printed "+today()+" · 0 changes since") {
		t.Error("identity strip does not read the fresh kit meta")
	}
}

// TestKitStalenessCountsInvalidatingWrites is the GATE for A3's staleness
// counter: the invalidating write verbs — dead-man arm, backup add-target,
// secret rotation — each bump the counter the identity strip renders as
// `{n} changes since`. Dropping a bump must red.
func TestKitStalenessCountsInvalidatingWrites(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.PutSetting(ctx, store.SettingKitPrintedAt, "2026-08-14"); err != nil {
		t.Fatalf("PutSetting printed: %v", err)
	}
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "db-password", Kind: "generic", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "db-password|generic",
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)

	// Arm the dead-man.
	resp := postForm(t, op, ts.URL+"/system/deadman/arm", url.Values{"threshold": {"48h"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deadman arm = %d, want 303", resp.StatusCode)
	}
	// Add a backup target.
	resp = postForm(t, op, ts.URL+"/system/backups/add",
		url.Values{"name": {"nas01"}, "kind": {"restic_local"}, "repository": {"local:/var/backups"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("backup add = %d, want 303", resp.StatusCode)
	}
	// Rotate a secret (reauth with the operator's own token).
	resp = postForm(t, op, ts.URL+"/secrets/db-password/rotate",
		url.Values{"value": {"v"}, "confirm_token": {tok}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("secret rotate = %d, want 303", resp.StatusCode)
	}

	raw, err := h.store.GetSetting(ctx, store.SettingKitChanges)
	if err != nil || raw != "3" {
		t.Fatalf("kit.changes = %q (%v), want 3", raw, err)
	}

	code, page := getBody(t, op, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, "kit printed 2026-08-14 · 3 changes since") {
		t.Error("identity strip does not read the staleness count")
	}
}

// TestKitMetaIdle: before a reprint, the identity strip renders the kit glyph
// alone — no fabricated date.
func TestKitMetaIdle(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `action="/system/kit/reprint"`) {
		t.Error("identity strip missing the reprint verb")
	}
	if strings.Contains(page, "changes since") {
		t.Error("identity strip fabricates kit meta before a print")
	}
}

// TestKitGlyphOpensModal is the GATE for the m97 kit modal's trigger: the
// Recovery-kit glyph is a data-modal-open verb (it OPENS the modal, it no
// longer posts the reprint directly), and the modal itself renders on /system.
func TestKitGlyphOpensModal(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`data-modal-open="kit"`,
		`title="Recovery kit — regenerate"`,
		`<dialog class="modal" id="kit"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("kit modal trigger missing %q", want)
		}
	}
}

// TestKitModalRendersKit is the GATE for the modal body: the regenerated kit —
// recipients, fingerprints, salt and the derivation recipe — renders inside the
// modal in a scrollable mono block with the shared Copy glyph, and a Download
// verb pointing at the GET route. The recovery secret NEVER renders (mutation:
// rendering AgeSecretKey here reds the guard).
func TestKitModalRendersKit(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`id="kit"`,
		`>RECOVERY KIT</p>`,
		`class="run-cmd-code"`,
		`class="copy-i"`,
		"1 · RECOVERY KEY (SECRET - THIS IS THE ROOT)",
		"The recovery age secret key is NOT shown here.",
		"2 · RECIPIENTS (PUBLIC - FOR VERIFICATION)",
		"3 · SALT (PUBLIC - NEEDED FOR EVERY DERIVATION)",
		"    " + secrets.Salt,
		"5 · DERIVATION RECIPE (GENERAL - ONE RULE)",
		`action="/system/kit/reprint"`,
		`href="/system/kit"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("kit modal missing %q", want)
		}
	}
	if strings.Contains(page, "AGE-SECRET-KEY-1") || strings.Contains(page, "exo_") {
		t.Error("kit modal leaked a recovery secret or access token")
	}
}

// TestKitDownloadServesAttachment is the GATE for the Download verb: GET
// /system/kit serves the raw rendered kit as text/plain with an attachment
// disposition, and the body carries the recipe but never a secret.
func TestKitDownloadServesAttachment(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp, err := op.Get(ts.URL + "/system/kit")
	if err != nil {
		t.Fatalf("GET /system/kit: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kit download = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("kit Content-Type = %q, want text/plain", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="recovery-kit.txt"` {
		t.Errorf("kit Content-Disposition = %q, want the attachment", cd)
	}
	for _, want := range []string{
		"exocomp · RECOVERY KIT",
		"2 · RECIPIENTS (PUBLIC - FOR VERIFICATION)",
		"3 · SALT (PUBLIC - NEEDED FOR EVERY DERIVATION)",
		"5 · DERIVATION RECIPE (GENERAL - ONE RULE)",
		"    exocomp kit derive restic/<target-id>",
		"kit sha256 · ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("downloaded kit missing %q", want)
		}
	}
	if strings.Contains(body, "AGE-SECRET-KEY-1") || strings.Contains(body, "exo_") {
		t.Error("downloaded kit leaked a recovery secret or access token")
	}
}

// TestKitPrimaryRecordsPrintSuccessFlash is the GATE for the modal's primary:
// it posts the existing reprint verb, and the redirect carries the SUCCESS-ONLY
// flash — the 303 sets the flash cookie to the one success sentence and the
// next /system render shows it beside the fresh kit meta.
func TestKitPrimaryRecordsPrintSuccessFlash(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/system/kit/reprint", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reprint status = %d, want 303", resp.StatusCode)
	}
	flashed := false
	for _, c := range resp.Cookies() {
		// The jar drops the em dash's bytes from the cookie value; the prefix
		// is the assertion that matters — the 303 carries the one success
		// sentence, never an error.
		if c.Name == flashCookie && strings.HasPrefix(c.Value, "recovery kit reprinted") {
			flashed = true
		}
	}
	if !flashed {
		t.Error("reprint 303 did not carry the success-only flash")
	}

	code, page := getBody(t, op, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, "recovery kit reprinted") {
		t.Error("the reprint flash did not render on /system")
	}
	if !strings.Contains(page, "kit printed "+today()+" · 0 changes since") {
		t.Error("identity strip does not read the fresh kit meta")
	}
}

// noKitDeriver is a Deriver with no envelope behind it: it can derive (so New
// accepts it) but it cannot render the kit.
type noKitDeriver struct{}

func (noKitDeriver) Derive(purpose, id string) []byte {
	return []byte("no-envelope:" + purpose + ":" + id)
}

func (noKitDeriver) Kit() (string, error) { return "", errors.New("no envelope") }

// TestKitRouteNoEnvelope503 is the GATE for the fail-close: a web-only Server
// holding no envelope (like the nil Sealer) answers the kit route 503.
func TestKitRouteNoEnvelope503(t *testing.T) {
	h := newHarness(t)
	nokit, err := New(Options{Store: h.store, Deriver: noKitDeriver{}, Version: "test", DataDir: h.dir})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(nokit.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, _ := getBody(t, client, ts.URL+"/system/kit")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("kit with nil envelope = %d, want 503", code)
	}
}
