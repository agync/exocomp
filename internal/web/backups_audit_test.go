package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// seedDrillJob enqueues one target's restore-drill job and waits for it to
// settle succeeded, returning the job id — seedDrillOK with the id kept, so a
// test can append the drill's outcome event to the very job it describes.
func seedDrillJob(t *testing.T, h *harness, name string) int64 {
	t.Helper()
	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "backup-drill", Kind: "shell", Trigger: "manual",
		Argv:           []string{"/bin/sh", "-c", "exit 0"},
		ConcurrencyKey: backup.DrillConcurrencyKey(name),
	})
	if err != nil {
		t.Fatalf("enqueue drill: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
	return job.ID
}

// drillOutcomeEvent is the info event a real restore drill emits for a good
// snapshot (internal/backup/drill.go DrillResult.String): the one trace a
// drill leaves — it never writes a backup_run row — and therefore A4's data
// source. 125829120 bytes = 120.0 MB.
const drillOutcomeEvent = "restore drill OK: restic_local:/var/backups/exocomp snapshot 2026-09-10T03:14:00Z, RPO 16h, 48 file(s) verified (125829120 bytes)"

// TestBackupsDrillOutcomeInLastRun is the GATE for A4 on the management list:
// the drill job's result (snapshot verified · restored bytes · duration) lands
// in the LAST RUN second line, parsed from the drill job's own outcome event.
// The exit code stays the judge; the parsed line only enriches the UI
// (invariant 3).
func TestBackupsDrillOutcomeInLastRun(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	drill := seedDrillJob(t, h, "nas01")
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{JobID: drill, Msg: drillOutcomeEvent}); err != nil {
		t.Fatalf("append drill outcome event: %v", err)
	}
	job, err := h.store.GetJob(context.Background(), drill)
	if err != nil {
		t.Fatalf("GetJob(drill): %v", err)
	}
	// The duration is the drill job's own wall time, rendered the same way the
	// LAST RUN cell renders a run's.
	expected := runDuration(job.FinishedAt.Sub(*job.StartedAt))

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	for _, want := range []string{
		"drill ", "ago ok",
		"120.0 MB verified",
		"120.0 MB verified · " + expected,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("LAST RUN drill outcome missing %q", want)
		}
	}
}

// TestBackupsDrillMarkWithoutOutcome pins the fallback: a succeeded drill whose
// job carries no outcome event still draws the plain "drill N ago ok" mark —
// never a fabricated bytes figure.
func TestBackupsDrillMarkWithoutOutcome(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	seedDrillJob(t, h, "nas01")

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, "drill ") || !strings.Contains(page, "ago ok") {
		t.Error("succeeded drill without an outcome event must still draw the plain mark")
	}
	if strings.Contains(page, "MB verified") {
		t.Error("a drill without an outcome event must not render a bytes figure")
	}
}

// TestBackupHistoryPageRows is the GATE for A12's surface: the target history
// page lists one snapshot per row (newest first), the drilled mark + outcome
// land on the newest row (A4's history face), and forget lives under each
// forgettable row's ▾ — ABSENT on the newest snapshot and on any drilled one.
func TestBackupHistoryPageRows(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "daily=7,weekly=4", true)
	seedRun(t, h, nas, store.BackupSucceeded, "c04b91dddddd222222222222", 1_100_000_000, 8*24*time.Hour)
	seedRun(t, h, nas, store.BackupSucceeded, "8e1d07cccccc111111111111", 1_100_000_000, 40*time.Hour)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	drill := seedDrillJob(t, h, "nas01")
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{JobID: drill, Msg: drillOutcomeEvent}); err != nil {
		t.Fatalf("append drill outcome event: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups/nas01/history")
	if code != http.StatusOK {
		t.Fatalf("history = %d, want 200", code)
	}
	// One row per snapshot, newest first; the drilled mark and its outcome on
	// the newest row.
	for _, want := range []string{
		"System / Backups / nas01",
		"#a3f9c2", "#8e1d07", "#c04b91",
		"drilled ", "ago ok",
		"120.0 MB verified",
		"/system/backups/nas01/forget",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("history page missing %q", want)
		}
	}
	// Forget under ▾ on the two forgettable rows, with the asks-first confirm.
	for _, want := range []string{
		`ramenu-btn">Forget snapshot #8e1d07cccccc</button>`,
		`ramenu-btn">Forget snapshot #c04b91dddddd</button>`,
		`data-confirm="Forget snapshot #8e1d07cccccc of nas01? restic forget removes it from the repository."`,
		`name="snapshot" value="8e1d07cccccc111111111111"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("forgettable row missing %q", want)
		}
	}
	// ABSENT on the newest snapshot (which is also the drilled one).
	for _, banned := range []string{
		`ramenu-btn">Forget snapshot #a3f9c2abcdef</button>`,
		`name="snapshot" value="a3f9c2abcdef1234567890ab"`,
	} {
		if strings.Contains(page, banned) {
			t.Errorf("newest/drilled snapshot must not draw a forget verb, found %q", banned)
		}
	}
}

// TestBackupForgetAsksFirst is the GATE for A12's confirm + guards: a forget
// POST without the confirm dialog's confirmed=1 is refused on the row, the
// newest snapshot and any foreign id are refused, and a confirmed forget of a
// forgettable snapshot enqueues ONE job through the one queue (invariant 5)
// with the target and snapshot id in its argv.
func TestBackupForgetAsksFirst(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "8e1d07cccccc111111111111", 1_100_000_000, 40*time.Hour)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// No confirmed → refused on the row, nothing enqueued.
	resp := postForm(t, client, ts.URL+"/system/backups/nas01/forget",
		url.Values{"snapshot": {"8e1d07cccccc111111111111"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed forget = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Nothing was forgotten — confirm the forget in the dialog.") {
		t.Error("unconfirmed forget refusal missing the confirm sentence")
	}

	// The newest snapshot is never forgettable, confirmed or not.
	resp = postForm(t, client, ts.URL+"/system/backups/nas01/forget",
		url.Values{"snapshot": {"a3f9c2abcdef1234567890ab"}, "confirmed": {"1"}}, nil)
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("newest-snapshot forget = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "The newest snapshot can&#39;t be forgotten — it is the recovery point.") {
		t.Error("newest-snapshot refusal missing the sentence")
	}

	// A snapshot that is not one of the target's rows is refused too.
	resp = postForm(t, client, ts.URL+"/system/backups/nas01/forget",
		url.Values{"snapshot": {"deadbeefdeadbeefdeadbeef"}, "confirmed": {"1"}}, nil)
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("foreign-snapshot forget = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "That snapshot is not on this target&#39;s history — refresh the page.") {
		t.Error("foreign-snapshot refusal missing the sentence")
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.ConcurrencyKey("nas01")})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("refused forgets enqueued a job (%d, err=%v)", len(jobs), err)
	}

	// The confirm dialog posts the row's snapshot and confirmed=1 → the one
	// queue gets the forget job.
	resp = postForm(t, client, ts.URL+"/system/backups/nas01/forget",
		url.Values{"snapshot": {"8e1d07cccccc111111111111"}, "confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed forget = %d, want 303", resp.StatusCode)
	}
	jobs, err = h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.ConcurrencyKey("nas01")})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("confirmed forget enqueued nothing (err=%v)", err)
	}
	job := jobs[len(jobs)-1]
	if job.Kind != "backup-forget" {
		t.Fatalf("forget job kind = %q, want backup-forget", job.Kind)
	}
	if len(job.Args.Argv) != 3 || job.Args.Argv[0] != "exocomp-forget" ||
		job.Args.Argv[1] != "nas01" || job.Args.Argv[2] != "8e1d07cccccc111111111111" {
		t.Fatalf("forget job argv = %v, want [exocomp-forget nas01 8e1d07…]", job.Args.Argv)
	}
}

// TestBackupsCardChipTwoFailing is the GATE for A10: `1 OF {m} FAILING` reads
// wait, but TWO failing targets read fail, not wait.
func TestBackupsCardChipTwoFailing(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedTarget(t, h, "b2-offsite", "restic_s3", "s3:bucket", "", true)
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", true)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// One failing target — wait.
	nas, _ := h.store.GetBackupTarget(context.Background(), "nas01")
	seedRun(t, h, nas, store.BackupFailed, "", 0, 2*time.Hour)
	_, page := getBody(t, client, ts.URL+"/system")
	for _, want := range []string{"1 OF 3 FAILING", `class="chip wait"`} {
		if !strings.Contains(page, want) {
			t.Errorf("one-failing card missing %q", want)
		}
	}

	// Two failing targets — fail, not wait.
	off, _ := h.store.GetBackupTarget(context.Background(), "b2-offsite")
	seedRun(t, h, off, store.BackupFailed, "", 0, 3*time.Hour)
	_, page = getBody(t, client, ts.URL+"/system")
	for _, want := range []string{"2 OF 3 FAILING", `class="chip fail"`} {
		if !strings.Contains(page, want) {
			t.Errorf("two-failing card missing %q", want)
		}
	}
}
