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

// drillOutcomeEvent is the human line a real restore drill emits for a good
// snapshot (internal/backup/drill.go DrillResult.String). It stays the
// readable trace; the web no longer text-parses it — the verified-bytes fact
// reads the structured payload instead, and a line that carries the figure
// renders nothing when raw_json is absent (TestBackupsDrillMarkWithoutOutcome).
// 125829120 bytes = 120.0 MB.
const drillOutcomeEvent = "restore drill OK: restic_local:/var/backups/exocomp snapshot 2026-09-10T03:14:00Z, RPO 16h, 48 file(s) verified (125829120 bytes)"

// drillOutcomeLine is the same drill's outcome line WITHOUT the byte figure.
// The raw_json tests seed this shape so the verified-bytes fact can only come
// from the structured payload, never the line (the old text-parse dies).
const drillOutcomeLine = "restore drill OK: restic_local:/var/backups/exocomp snapshot 2026-09-10T03:14:00Z, RPO 16h"

// drillOutcomeRawJSON is the structured payload the drill job attaches to the
// outcome event's raw_json (internal/backup/drill.go DrillEvent): the A4 bytes
// fact's source. 125829120 bytes = 120.0 MB.
const drillOutcomeRawJSON = `{"backup.drill":{"source":"restic_local:/var/backups/exocomp","snapshot":"2026-09-10T03:14:00Z","rpo_age":57600,"ok":true,"verified_files":48,"verified_bytes":125829120}}`

// TestBackupsDrillOutcomeInLastRun is the GATE for A4 on the management list:
// the drill job's result (snapshot verified · restored bytes · duration) lands
// in the LAST RUN second line. The bytes fact reads the job's outcome event's
// raw_json payload — the seeded line carries no byte figure, so the figure
// CANNOT come from text-parsing it. The exit code stays the judge; the payload
// only enriches the UI (invariant 3).
func TestBackupsDrillOutcomeInLastRun(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	drill := seedDrillJob(t, h, "nas01")
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{JobID: drill, Msg: drillOutcomeLine, RawJSON: drillOutcomeRawJSON}); err != nil {
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

// TestBackupsDrillMarkWithoutOutcome pins the raw_json dependency: a
// succeeded drill whose outcome event carries ONLY the human line — no
// structured payload — still draws the plain "drill N ago ok" mark but never
// a bytes figure, even though the line itself holds the byte count. This is
// the mutation the "reads raw_json, not the line" guard closes: blank the
// payload and the fact goes empty.
func TestBackupsDrillMarkWithoutOutcome(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	drill := seedDrillJob(t, h, "nas01")
	// The line still carries "verified (125829120 bytes)" — but the web must
	// not read it: no raw_json payload, no bytes fact.
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{JobID: drill, Msg: drillOutcomeEvent}); err != nil {
		t.Fatalf("append drill outcome event: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, "drill ") || !strings.Contains(page, "ago ok") {
		t.Error("succeeded drill without a payload must still draw the plain mark")
	}
	if strings.Contains(page, "MB verified") {
		t.Error("a drill without a raw_json payload must not render a bytes figure")
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
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{JobID: drill, Msg: drillOutcomeLine, RawJSON: drillOutcomeRawJSON}); err != nil {
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
	// the newest row. The breadcrumb lives in the topbar title (R8b), never a
	// card kicker.
	for _, want := range []string{
		`<h1 class="appbar-title">System · Backups · nas01</h1>`,
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

// TestBackupDrawerEchoUsesTargetKind is the cross-lane contract's web face:
// backupSnapshots fills each drawer row's Kind from its target, and the
// confirmed drawer's RUN ON THE HOST command names --snapshot {id} for a
// selected non-newest restic snapshot — driven through the real data path,
// not a hand-built snapshotRow — while a git target's non-newest row stays
// plain (git's force-push makes its older rows phantom; the echo must not
// pretend).
func TestBackupDrawerEchoUsesTargetKind(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "daily=7,weekly=4", true)
	seedRun(t, h, nas, store.BackupSucceeded, "8e1d07cccccc111111111111", 1_100_000_000, 40*time.Hour)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	git := seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", true)
	seedRun(t, h, git, store.BackupSucceeded, "9f4b11eeeeee222222222222", 900_000_000, 70*time.Hour)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// An older restic snapshot selected: the command names --snapshot {id}.
	resp := postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"snapshot"}, "snapshot": {"8e1d07cccccc"}, "confirm": {"8e1d07cccccc"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("older-restic confirm = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, ">exocomp restore --snapshot 8e1d07cccccc</pre>") {
		t.Errorf("older restic row must echo the --snapshot selector: %q", body)
	}

	// A git target's non-newest row stays plain: nothing to select.
	resp = postForm(t, client, ts.URL+"/system/backups/restore", url.Values{
		"from": {"snapshot"}, "snapshot": {"9f4b11eeeeee"}, "confirm": {"9f4b11eeeeee"},
	}, nil)
	body = readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("git confirm = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, ">exocomp restore</pre>") {
		t.Errorf("git non-newest row must echo the plain restore command")
	}
	if strings.Contains(body, "--snapshot") {
		t.Errorf("git row must not echo --snapshot: %q", body)
	}
}
