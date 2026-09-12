package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
