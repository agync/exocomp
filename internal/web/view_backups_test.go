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

// seedTarget registers one backup target with the given posture.
func seedTarget(t *testing.T, h *harness, name, kind, uri, retention string, enabled bool) store.BackupTarget {
	t.Helper()
	tr, err := h.store.CreateBackupTarget(context.Background(), store.BackupTarget{
		Name: name, Kind: kind, URI: uri, Retention: retention, Enabled: enabled,
	})
	if err != nil {
		t.Fatalf("CreateBackupTarget(%s): %v", name, err)
	}
	return tr
}

// seedRun records a settled backup run for a target at a fixed age.
func seedRun(t *testing.T, h *harness, target store.BackupTarget, state, snapshot string, added int64, ago time.Duration) {
	t.Helper()
	run, err := h.store.StartBackupRun(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("StartBackupRun(%s): %v", target.Name, err)
	}
	finished := time.Now().UTC().Add(-ago)
	run.State = state
	run.SnapshotID = snapshot
	run.BytesAdded = added
	run.FinishedAt = &finished
	if state == store.BackupFailed {
		run.Error = "restic backup exited 1: repository locked by another process"
	}
	if err := h.store.FinishBackupRun(context.Background(), run); err != nil {
		t.Fatalf("FinishBackupRun(%s): %v", target.Name, err)
	}
}

// seedDrillOK records a succeeded restore-drill job for a target (a shell job
// under the target's drill concurrency key, so the harness worker settles it
// succeeded deterministically).
func seedDrillOK(t *testing.T, h *harness, name string) {
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
}

// seedRunningRun creates a RUNNING backup_run plus a job under the target's
// backup concurrency key, so the A9 `● RUNNING · job #…` row has a job id.
func seedRunningRun(t *testing.T, h *harness, target store.BackupTarget) int64 {
	t.Helper()
	if _, err := h.store.StartBackupRun(context.Background(), target.ID); err != nil {
		t.Fatalf("StartBackupRun(%s): %v", target.Name, err)
	}
	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "backup", Kind: "shell", Trigger: "manual",
		Argv:           []string{"/bin/sh", "-c", "exit 0"},
		ConcurrencyKey: backup.ConcurrencyKey(target.Name),
	})
	if err != nil {
		t.Fatalf("enqueue run job: %v", err)
	}
	return job.ID
}

// TestBackupsListColumns is the GATE for the /system/backups management list
// (SPEC §4.1): the six columns render, an ENABLED row carries the run verb +
// chevron, a DISABLED row carries NO run verb (only the chevron), retention
// and schedule cells read the stored policy, and the LAST RUN cell draws a
// succeeded run with its drill mark.
func TestBackupsListColumns(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_290_000_000, 16*time.Hour)
	seedDrillOK(t, h, "nas01")
	off := seedTarget(t, h, "b2-offsite", "restic_s3", "s3:s3.us-west-002.backblazeb2.com/exocomp", "none", true)
	seedRun(t, h, off, store.BackupFailed, "", 0, 61*time.Hour)
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", false)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	// The header verbs (SPEC §4.1): Restore (danger ring) and Add target.
	for _, want := range []string{
		"Restore", "Add target", `data-modal-open="restore-backups"`, `data-modal-open="backups-add"`,
		">TARGET</th>", ">KIND</th>", ">RETENTION</th>", ">SCHEDULE</th>", ">LAST RUN</th>", ">ACTIONS</th>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("backups list missing %q", want)
		}
	}
	// The enabled nas01 row: kind, retention default (daily 7 · weekly 4),
	// the run verb, the succeeded run with its drill mark.
	for _, want := range []string{
		"nas01", "restic_local", "daily 7 · weekly 4", "prunes after run",
		`title="Queue backup — nas01"`,
		`aria-label="Queue backup — nas01"`,
		"SUCCEEDED", "#a3f9c2", "drill ",
		"ago ok",
		"/system/backups/nas01/run",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("nas01 row missing %q", want)
		}
	}
	// The failed b2-offsite row: FAILED, the reason, keep-all's honest
	// consequence, the run verb still present on an enabled row.
	for _, want := range []string{
		"b2-offsite", "restic_s3", "keep all", "never prunes · grows",
		"FAILED", "repository locked by another process",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("b2-offsite row missing %q", want)
		}
	}
	// The disabled git-config row: DISABLED, git retention, no run verb, the
	// enable verb lives in the menu (chevron only).
	for _, want := range []string{
		"git-config", "DISABLED", "git keeps history", "no schedule", "add one",
		`action="/system/backups/git-config/enable"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("git-config row missing %q", want)
		}
	}
	// The mutation: the run verb is ABSENT on the disabled row.
	for _, want := range []string{
		`title="Queue backup — git-config"`,
		"/system/backups/git-config/run",
	} {
		if strings.Contains(page, want) {
			t.Errorf("disabled row must not draw a run verb, found %q", want)
		}
	}
}

// TestBackupsListNeverEcho is the GATE for invariant 4 on this surface: a
// target whose --uri embeds credentials renders scrubbed (***@), never the
// token, and the Add dialog carries no password field at all.
func TestBackupsListNeverEcho(t *testing.T) {
	h := newHarness(t)
	const token = "SECRETTOKENvalue"
	seedTarget(t, h, "nas01", "restic_s3", "s3:https://"+token+"@s3.example/bucket", "", true)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, path := range []string{"/system/backups", "/system"} {
		code, page := getBody(t, client, ts.URL+path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, code)
		}
		if strings.Contains(page, token) {
			t.Errorf("%s echoed the uri credential %q", path, token)
		}
	}
	// The scrubbed uri renders on the management list's KIND cell; the card on
	// /system never draws the uri at all.
	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, "https://***@s3.example/bucket") {
		t.Error("the list did not render the scrubbed uri")
	}
	if strings.Contains(page, `type="password"`) {
		t.Error("the backups surface has no password field — the repository password is derived on the daemon side")
	}
}

// TestBackupsListRunningRow is the GATE for A9 on the management list: a
// RUNNING run draws `● RUNNING · {elapsed} · job #{id}`, and the card on
// /system draws `● RUNNING · job #…` and states that a running run does not
// move the age.
func TestBackupsListRunningRow(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	seedRunningRun(t, h, nas)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	for _, want := range []string{
		"RUNNING", "job #",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("running row missing %q", want)
		}
	}

	code, card := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		">RUNNING</span>",
		"job #",
		`href="/system/backups"`, ">Manage</a>",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("backups card missing %q", want)
		}
	}
}

// TestBackupsAddDialogOneVerb is the GATE for §4.2's one verb: the Add-target
// dialog renders Name · Kind (three real kinds — the B-1 fold cut restic_sftp,
// which no engine or CLI handles) · Repository · Retention · Schedule, with
// the primary "Add and queue first backup — {name}".
func TestBackupsAddDialogOneVerb(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	for _, want := range []string{
		`id="backup-add-form"`,
		"Add a backup target",
		"Add and queue first backup — ",
		`value="restic_local"`, `value="restic_s3"`, `value="git"`,
		`name="repository"`,
		`name="daily"`, `name="weekly"`, `name="keep_all"`,
		`name="schedule"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("add dialog missing %q", want)
		}
	}
	// The B-1 fold: the SPEC's restic_sftp over-claim is cut — no radio, no
	// CLI echo.
	if strings.Contains(page, "restic_sftp") {
		t.Error("add dialog must not offer the restic_sftp kind (B-1 cut)")
	}
}

// TestBackupAddQueuesFirstBackup is the GATE for §4.2's one verb behaviour:
// Add registers the target, creates its kind=backup schedule, AND queues the
// first backup — all three, nothing half-done.
func TestBackupAddQueuesFirstBackup(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/add", url.Values{
		"name": {"hetzner-box"}, "kind": {"restic_s3"},
		"repository": {"s3:s3.us-west-002.backblazeb2.com/exocomp"},
		"daily":      {"7"}, "weekly": {"4"}, "schedule": {"0 2 * * * *"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add = %d, want 303", resp.StatusCode)
	}

	tr, err := h.store.GetBackupTarget(context.Background(), "hetzner-box")
	if err != nil {
		t.Fatalf("target not created: %v", err)
	}
	if !tr.Enabled || tr.Retention != "daily=7,weekly=4" {
		t.Fatalf("created target = enabled=%v retention=%q, want enabled with daily=7,weekly=4", tr.Enabled, tr.Retention)
	}
	sc, err := h.store.GetSchedule(context.Background(), "hetzner-box")
	if err != nil {
		t.Fatalf("schedule not created: %v", err)
	}
	if sc.Kind != store.ScheduleBackup || sc.Args.Target != "hetzner-box" {
		t.Fatalf("schedule = kind=%q target=%q, want backup/hetzner-box", sc.Kind, sc.Args.Target)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.ConcurrencyKey("hetzner-box")})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("first backup not queued (err=%v)", err)
	}
	if jobs[len(jobs)-1].Kind != backup.JobKind {
		t.Fatalf("first backup job kind = %q, want %q", jobs[len(jobs)-1].Kind, backup.JobKind)
	}
}

// TestBackupAddRefusals pins the Add dialog's human refusals: a bad name, a
// fabricated kind (restic_sftp), and a bad schedule each re-render the dialog
// with a sentence — never a flash.
func TestBackupAddRefusals(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"empty name", url.Values{"kind": {"git"}, "repository": {"git@host:r.git"}}, "Give the target a name."},
		{"bogus kind", url.Values{"name": {"x"}, "kind": {"restic_sftp"}, "repository": {"s3:b"}}, "Kind is restic_local, restic_s3 or git."},
		{"empty repo", url.Values{"name": {"x"}, "kind": {"restic_local"}}, "Give a repository"},
		{"bad schedule", url.Values{"name": {"x"}, "kind": {"restic_local"}, "repository": {"/r"}, "schedule": {"not a cron"}}, "be parsed — try 0 2 * * * * or @every 24h"},
		{"bad daily", url.Values{"name": {"x"}, "kind": {"restic_local"}, "repository": {"/r"}, "daily": {"lots"}}, "Retention counts are whole numbers"},
	}
	for _, tc := range cases {
		resp := postForm(t, client, ts.URL+"/system/backups/add", tc.form, nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: add = %d, want 422", tc.name, resp.StatusCode)
			continue
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: refusal missing %q", tc.name, tc.want)
		}
	}
	// Nothing was created by any refused submit.
	targets, err := h.store.ListBackupTargets(context.Background(), true)
	if err != nil || len(targets) != 0 {
		t.Fatalf("refused adds created targets: %d (err=%v)", len(targets), err)
	}
}

// TestBackupRunVerb is the GATE for the run verb: it enqueues a kind=backup
// job through the one queue (invariant 5) and flashes the job id.
func TestBackupRunVerb(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/nas01/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("run = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.ConcurrencyKey("nas01")})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("run job not enqueued (err=%v)", err)
	}
	if jobs[len(jobs)-1].Kind != backup.JobKind {
		t.Fatalf("run job kind = %q, want %q", jobs[len(jobs)-1].Kind, backup.JobKind)
	}
}

// TestBackupRunDisabledRefusal is the round's mutation gate: a crafted run
// POST against a DISABLED target is refused on the row with a human sentence,
// never a flash, and nothing is enqueued.
func TestBackupRunDisabledRefusal(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", false)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/git-config/run", url.Values{}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("disabled run = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Target git-config is disabled — enable it before running a backup.") {
		t.Error("disabled-run refusal missing the human sentence")
	}
	if strings.Contains(body, "queued backup") {
		t.Error("disabled run must not flash a queue success")
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.ConcurrencyKey("git-config")})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("disabled run enqueued a job (%d, err=%v)", len(jobs), err)
	}
}

// TestBackupEnableDisable flips a target's ENABLED/DISABLED word both ways.
func TestBackupEnableDisable(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct {
		verb string
		want bool
	}{
		{"disable", false},
		{"enable", true},
	} {
		resp := postForm(t, client, ts.URL+"/system/backups/nas01/"+tc.verb, url.Values{}, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s = %d, want 303", tc.verb, resp.StatusCode)
		}
		tr, err := h.store.GetBackupTarget(context.Background(), "nas01")
		if err != nil {
			t.Fatalf("GetBackupTarget: %v", err)
		}
		if tr.Enabled != tc.want {
			t.Errorf("after %s, enabled = %v, want %v", tc.verb, tr.Enabled, tc.want)
		}
	}
}

// TestBackupRetentionEdit writes the edit-retention dialog's cells into the
// stored policy, and refuses a nonsense count on the dialog.
func TestBackupRetentionEdit(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "daily=7,weekly=4", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/nas01/retention",
		url.Values{"daily": {"14"}, "weekly": {"8"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retention = %d, want 303", resp.StatusCode)
	}
	tr, err := h.store.GetBackupTarget(context.Background(), "nas01")
	if err != nil {
		t.Fatalf("GetBackupTarget: %v", err)
	}
	if tr.Retention != "daily=14,weekly=8" {
		t.Fatalf("retention = %q, want daily=14,weekly=8", tr.Retention)
	}

	// keep-all writes the none spec.
	resp = postForm(t, client, ts.URL+"/system/backups/nas01/retention",
		url.Values{"keep_all": {"1"}}, nil)
	_ = resp.Body.Close()
	tr, _ = h.store.GetBackupTarget(context.Background(), "nas01")
	if tr.Retention != "none" {
		t.Fatalf("keep-all retention = %q, want none", tr.Retention)
	}

	// A bad count refuses on the dialog.
	resp = postForm(t, client, ts.URL+"/system/backups/nas01/retention",
		url.Values{"daily": {"-3"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad retention = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Retention counts can&#39;t be negative.") {
		t.Error("bad retention refusal missing the human sentence")
	}
}

// TestBackupRemoveAsksFirst pins the remove verb's confirm gate: a
// confirmed-less remove refuses on the row, a confirmed one deletes.
func TestBackupRemoveAsksFirst(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/nas01/remove", url.Values{}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed remove = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Nothing was removed — confirm the removal in the dialog.") {
		t.Error("unconfirmed remove refusal missing the confirm sentence")
	}
	if _, err := h.store.GetBackupTarget(context.Background(), "nas01"); err != nil {
		t.Fatalf("unconfirmed remove deleted the target: %v", err)
	}

	resp = postForm(t, client, ts.URL+"/system/backups/nas01/remove",
		url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed remove = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetBackupTarget(context.Background(), "nas01"); err == nil {
		t.Fatal("confirmed remove did not delete the target")
	}
}

// TestBackupsLastRunPhoneGap pins the LAST RUN cell's phone form: the cell
// stacks its label, chip+reason line and meta instead of squeezing them into
// one flex row, the chip holds its width so the reason wraps inside the cell,
// and the reason's nowrap/ellipsis override is lifted so the chip never
// overlaps it. Layout is browser-verified; this guard pins the rules. The
// mutation (dropping the stack or the chip's flex:none) reds this test.
func TestBackupsLastRunPhoneGap(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupFailed, "", 0, 11*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `<span class="lr-line">`) {
		t.Error("the LAST RUN cell must keep its two-line stack")
	}
	app := cssOf(t, h, ts, client, "app.css")
	for _, want := range []string{
		".tablewrap.stackable .bk-lastrun { flex-direction: column;",
		".bk-lastrun .lr-line .chip { flex: none; }",
		".bk-lastrun .lr-line .lr-text {",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the LAST RUN phone rule %q", want)
		}
	}
}

// TestBackupDrillVerb enqueues a restore-drill job through the one queue, the
// same kind a kind=drill schedule fire enqueues.
func TestBackupDrillVerb(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/nas01/drill", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("drill = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{ConcurrencyKey: backup.DrillConcurrencyKey("nas01")})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("drill job not enqueued (err=%v)", err)
	}
	if jobs[len(jobs)-1].Kind != backup.DrillJobKind {
		t.Fatalf("drill job kind = %q, want %q", jobs[len(jobs)-1].Kind, backup.DrillJobKind)
	}
}
