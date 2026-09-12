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

// seedSnapshotTarget registers a target with one succeeded snapshot run at a
// fixed age and returns the snapshot's short id (the first 12 chars — the
// same cut the list and the typed confirm use).
func seedSnapshotTarget(t *testing.T, h *harness, name string, snapshot string, ago time.Duration) string {
	t.Helper()
	tr := seedTarget(t, h, name, "restic_local", "/var/backups/"+name, "daily=7,weekly=4", true)
	seedRun(t, h, tr, store.BackupSucceeded, snapshot, 1_200_000_000, ago)
	return snapshot[:12]
}

// TestBackupRestoreDrawerRenders is the GATE for the DR drawer's shape (SPEC
// §4.3): kicker, lede, the From switch with its meta, snapshot radios
// newest-first across targets with the drilled ok mark, WHAT HAPPENS with the
// loss in fail, the typed confirm, the danger button, and the CLI echo incl.
// --from-litestream.
func TestBackupRestoreDrawerRenders(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	seedDrillOK(t, h, "nas01")
	seedSnapshotTarget(t, h, "b2-offsite", "8e1d07cccccc111111111111", 40*time.Hour)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	for _, want := range []string{
		"SYSTEM · BACKUPS · DISASTER RECOVERY",
		"This replaces exocomp's own store.",
		"restore-backups-form",
		`value="snapshot"`, `value="replica"`,
		"verified by the drill",
		`name="snapshot"`,
		"#a3f9c2", "nas01", "b2-offsite", "#8e1d07",
		"drilled", "ago ok",
		"newest first across every target",
		"the queue drains", "waiting approval",
		"are lost",
		"Type the snapshot id to confirm",
		"the dialog is the safety — no second confirm after this",
		"Restore from #a3f9c2",
		"exocomp restore --from-litestream",
		"recovery kit",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("restore drawer missing %q", want)
		}
	}
	// The danger button rings until the typed id matches; it carries the
	// expected confirm value as its contract.
	if !strings.Contains(page, `id="restore-submit"`) {
		t.Error("missing the danger submit button")
	}
	if !strings.Contains(page, "restore-backups") {
		t.Error("missing the drawer id")
	}
}

// TestBackupRestoreTypedConfirm is the GATE for the round's hard rule (c): a
// typed id that does not match the selected snapshot is refused on the drawer
// with the human sentence (never a flash); the matching id passes and names
// the recovery-kit CLI path.
func TestBackupRestoreTypedConfirm(t *testing.T) {
	h := newHarness(t)
	id := seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// Wrong id — the mutation gate.
	resp := postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"snapshot"}, "snapshot": {id}, "confirm": {"bogus-id"},
	}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong-id restore = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Type the snapshot id shown on the selected snapshot to confirm.") {
		t.Error("wrong-id refusal missing the human sentence")
	}
	if !strings.Contains(body, "restore-backups-form") {
		t.Error("wrong-id refusal must re-render the drawer, not flash")
	}
	if strings.Contains(body, "restore from") {
		t.Error("wrong-id restore must not flash a success")
	}

	// Empty confirm — same gate.
	resp = postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"snapshot"}, "snapshot": {id},
	}, nil)
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty-confirm restore = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Type the snapshot id shown on the selected snapshot to confirm.") {
		t.Error("empty-confirm refusal missing the human sentence")
	}

	// Correct id — the gate passes; the live daemon cannot replace its own
	// store, so the verb names the recovery-kit CLI path.
	resp = postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"snapshot"}, "snapshot": {id}, "confirm": {id},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct-id restore = %d, want 303", resp.StatusCode)
	}
	// The flash names the CLI path (recovery kit), never a pretend success.
	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, "run exocomp restore --snapshot "+id+" on the host (recovery kit)") {
		t.Error("confirmed restore flash missing the recovery-kit CLI path")
	}
}

// TestBackupRestoreReplicaMode is the contract m76's litestream leg keys off:
// ?restore=replica opens the drawer in replica mode (typed confirm = the word
// "replica", button "Restore from replica", echo --from-litestream), and the
// same typed-confirm gate refuses a wrong word.
func TestBackupRestoreReplicaMode(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups?restore=replica")
	if code != http.StatusOK {
		t.Fatalf("deep link = %d, want 200", code)
	}
	for _, want := range []string{
		"Restore from replica",
		"Type the source to confirm",
		"exocomp restore --from-litestream",
		`value="replica" checked`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("replica drawer missing %q", want)
		}
	}

	// Wrong word — refused by the same gate.
	resp := postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"replica"}, "snapshot": {""}, "confirm": {"snapshot"},
	}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong replica confirm = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Type the snapshot id shown on the selected snapshot to confirm.") {
		t.Error("replica wrong-confirm refusal missing the sentence")
	}

	// The word "replica" passes.
	resp = postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"replica"}, "confirm": {"replica"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("replica confirm = %d, want 303", resp.StatusCode)
	}
}

// TestBackupsSnapshotListNewestFirst pins the S-3 read: the drawer's radios
// list snapshots across targets, newest first, and the drilled ok mark lands
// on the newest snapshot of a drilled target only.
func TestBackupsSnapshotListNewestFirst(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "daily=7,weekly=4", true)
	seedRun(t, h, nas, store.BackupSucceeded, "8e1d07cccccc111111111111", 1_100_000_000, 40*time.Hour)
	seedDrillOK(t, h, "nas01")
	newID := "a3f9c2abcdef"
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	old := "8e1d07cccccc"
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", false)

	ctx := context.Background()
	snaps, err := h.server.backupSnapshots(ctx)
	if err != nil {
		t.Fatalf("backupSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshot count = %d, want 2 (git targets have no snapshot runs)", len(snaps))
	}
	// Newest first.
	if snaps[0].ID != newID || snaps[1].ID != old {
		t.Fatalf("snapshot order = [%s %s], want newest-first [%s %s]", snaps[0].ID, snaps[1].ID, newID, old)
	}
	// The drilled ok mark lands on the newest snapshot of the drilled target.
	if !snaps[0].Drilled {
		t.Error("newest snapshot of a drilled target must carry the ok mark")
	}
	if snaps[1].Drilled {
		t.Error("older snapshots of a drilled target must not carry the ok mark (a drill verifies the newest at drill time)")
	}
	if snaps[0].Target != "nas01" || snaps[1].Target != "nas01" {
		t.Errorf("snapshot targets wrong: %s / %s", snaps[0].Target, snaps[1].Target)
	}
}
