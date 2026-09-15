package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// updateCmd is update that also returns the command a key produced, so a test
// can run the write action (a tea.Cmd goroutine) and feed its message back.
func updateCmd(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return got, cmd
}

// newTestWrite mints a real envelope in a temp dir and returns the Write
// surface both the test and the code-under-test open from it.
func newTestWrite(t *testing.T) (*Write, *secrets.Envelope) {
	t.Helper()
	dir := t.TempDir()
	env, _, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("init envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	return &Write{
		OpenEnvelope: func() (*secrets.Envelope, error) { return secrets.Open(dir) },
		DataDir:      dir,
	}, env
}

// seedGatedJob creates a job sitting in awaiting_approval, the state no worker
// claims.
func seedGatedJob(t *testing.T, st *store.Store) store.Job {
	t.Helper()
	job, err := st.CreateGatedJob(context.Background(), store.NewJob{
		Runbook: "rb", Kind: "tofu", Trigger: "reactive",
	})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	return job
}

// seedApproval derives the job's capability token from the envelope and
// records the pending approval the TUI is asked to decide.
func seedApproval(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64, workspace string, planSerial int64, planArtifact *int64) store.Approval {
	t.Helper()
	_, hash, err := reactive.ApprovalToken(env, jobID)
	if err != nil {
		t.Fatalf("approval token: %v", err)
	}
	now := time.Now().UTC()
	a, err := st.CreateApproval(context.Background(), store.Approval{
		JobID: jobID, Workspace: workspace, PlanSerial: planSerial, PlanArtifact: planArtifact,
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), TokenHash: hash,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	return a
}

// binaryPlanBlob stands in for the binary `.tfplan` tofu writes: hand it to a
// JSON parser and nothing comes back, exactly the production failure the
// companion artifact exists to avoid.
var binaryPlanBlob = []byte("PK\x03\x04\x00not-json")

// sealBlob seals raw bytes through the envelope and returns the sealed path.
func sealBlob(t *testing.T, env *secrets.Envelope, name string, blob []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	sealed, err := env.SealFile(path)
	if err != nil {
		t.Fatalf("seal %s: %v", name, err)
	}
	return sealed
}

// seedBinaryPlanArtifact records a binary stand-in for the saved plan under
// reactive.PlanArtifactKind — the artifact the approval references and the one
// the TUI must NOT parse. It returns the artifact's ID.
func seedBinaryPlanArtifact(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64) *int64 {
	t.Helper()
	art, err := st.CreateArtifact(context.Background(), store.Artifact{
		JobID: jobID, Kind: reactive.PlanArtifactKind, Path: sealBlob(t, env, "plan.tfplan", binaryPlanBlob),
	})
	if err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	return &art.ID
}

// seedShowArtifact records a sealed `tofu show -json` companion on the same job
// — the document the diff actually renders.
func seedShowArtifact(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64, doc string) {
	t.Helper()
	if _, err := st.CreateArtifact(context.Background(), store.Artifact{
		JobID: jobID, Kind: reactive.PlanShowArtifactKind, Path: sealBlob(t, env, "plan-show.json", []byte(doc)),
	}); err != nil {
		t.Fatalf("create show artifact: %v", err)
	}
}

// seedPlanArtifact seeds a plan the way the executor records one: a binary
// saved plan under reactive.PlanArtifactKind plus a sealed show-json companion
// on the same job. It returns the saved plan's ID, which the approval
// references. destroys chooses a plan that tears a resource down or one that
// only creates.
func seedPlanArtifact(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64, destroys bool) *int64 {
	t.Helper()
	var changes string
	if destroys {
		changes = `[{"address":"aws_db_instance.legacy_reports","change":{"actions":["delete"],"after":{},"after_sensitive":{},"before_sensitive":{}}}]`
	} else {
		changes = `[{"address":"aws_s3_bucket.reports-archive","change":{"actions":["create"],"after":{},"after_sensitive":{},"before_sensitive":{}}}]`
	}
	seedShowArtifact(t, st, env, jobID, fmt.Sprintf(`{"resource_changes":%s}`, changes))
	return seedBinaryPlanArtifact(t, st, env, jobID)
}

// seedOperatorPlanArtifact seals a show-json document under the saved-plan kind
// on a job with no companion — a plan sealed before the capture existed. It is
// the fallback shape. It returns the plan artifact's ID.
func seedOperatorPlanArtifact(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64) *int64 {
	t.Helper()
	doc := `{"resource_changes":[{"address":"aws_db_instance.legacy_reports","change":{"actions":["delete"],"after":{},"after_sensitive":{},"before_sensitive":{}}}]}`
	art, err := st.CreateArtifact(context.Background(), store.Artifact{
		JobID: jobID, Kind: reactive.PlanArtifactKind, Path: sealBlob(t, env, "operator-plan.json", []byte(doc)),
	})
	if err != nil {
		t.Fatalf("create operator plan artifact: %v", err)
	}
	return &art.ID
}

// runCmd executes a tea.Cmd's message and feeds it back through Update, the
// same hop the Bubble Tea runtime makes.
func runCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	return update(t, m, cmd())
}

// seedSensitivePlanArtifact seeds a plan whose *_sensitive trees carry a true
// flag and a secret value, the fixture the [d] pager redaction test drives.
func seedSensitivePlanArtifact(t *testing.T, st *store.Store, env *secrets.Envelope, jobID int64) *int64 {
	t.Helper()
	changes := `[{"address":"aws_db_instance.legacy_reports","change":{"actions":["update"],` +
		`"after":{"engine":"pg14"},"after_sensitive":{"password":"s3cr3t-p@ss","sensitive":true},` +
		`"before_sensitive":{"password":"s3cr3t-p@ss","sensitive":true}}}]`
	seedShowArtifact(t, st, env, jobID, fmt.Sprintf(`{"resource_changes":%s}`, changes))
	return seedBinaryPlanArtifact(t, st, env, jobID)
}

// TestNilWriteDisablesWriteKeys proves the read-only posture: with a nil Write
// every write key is a no-op even when its screen carries a real row to act on,
// and no command or overlay ever appears, so the store is left untouched.
// Seeding each surface (a queued job, a gated approval, a backup target, a
// workspace, a schedule) keeps the keys reachable, so stripping any one guard
// goes red. d is read-only (its pager opens with nil Write).
func TestNilWriteDisablesWriteKeys(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	_, env := newTestWrite(t)
	gated := seedGatedJob(t, st)
	seedApproval(t, st, env, gated.ID, "core", 1, nil)
	seedJob(t, st, "ansible", store.StateQueued, nil)
	if _, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "restic-local", Kind: "restic_local", URI: "/var/lib/exocomp/restic", Enabled: true,
	}); err != nil {
		t.Fatalf("create backup target: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "core", TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "@daily", Kind: store.ScheduleBackup,
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	before := len(listJobs(t, st))

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadJobsCmd(st)())
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, loadBackupsCmd(st)())
	m = update(t, m, loadWorkspacesCmd(st)())
	m = update(t, m, loadSchedulesCmd(st)())

	assertInert := func(key tea.KeyPressMsg, m Model) {
		t.Helper()
		got, cmd := updateCmd(t, m, key)
		if cmd != nil {
			t.Fatalf("key %q with nil Write produced a command; write keys must be inert", key.String())
		}
		if got.modal != modalNone || got.helpOpen {
			t.Fatalf("key %q with nil Write opened an overlay", key.String())
		}
	}

	// Home: p pauses, m mutes.
	for _, k := range []tea.KeyPressMsg{key('p'), key('m')} {
		assertInert(k, m)
	}

	// Approvals: a approves, r rejects.
	m = update(t, m, key('5'))
	for _, k := range []tea.KeyPressMsg{key('a'), key('r')} {
		assertInert(k, m)
	}

	// Jobs: c cancels the selected queued job.
	m = update(t, m, key('1'))
	assertInert(key('c'), m)

	// Backups: b runs the selected target now.
	m = update(t, m, key('2'))
	assertInert(key('b'), m)

	// Workspaces: l plans the selected workspace now.
	m = update(t, m, key('3'))
	assertInert(key('l'), m)

	// Schedules: space toggles, x runs the selected schedule now.
	m = update(t, m, key('4'))
	assertInert(key(' '), m)
	assertInert(key('x'), m)

	if after := len(listJobs(t, st)); after != before {
		t.Fatalf("nil Write enqueued jobs: count %d -> %d, want unchanged", before, after)
	}
}

// TestPauseAndResume drives p through the mandatory-expiry prompt into a
// global silence and back out again, the banner's two states.
func TestPauseAndResume(t *testing.T) {
	st := openTestStore(t)
	m := New(st, &Write{})
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())

	m, _ = updateCmd(t, m, key('p'))
	if m.modal != modalPause {
		t.Fatalf("p did not open the pause prompt, modal = %v", m.modal)
	}
	for _, r := range "45" {
		m = update(t, m, key(r))
	}
	m = runCmd(t, m, updateCmdNoCheck(t, m, specialKey(tea.KeyEnter)))
	sils, err := st.ListSilences(context.Background(), true)
	if err != nil || len(sils) != 1 || sils[0].ScopeKind != store.ScopeGlobal {
		t.Fatalf("after pause: silences = %+v (%v), want one global", sils, err)
	}
	if !sils[0].ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("pause expiry %v is not ~45 minutes out", sils[0].ExpiresAt)
	}

	// The banner reloads the glance; p now resumes.
	m = update(t, m, loadGlanceCmd(st)())
	m = runCmd(t, m, updateCmdNoCheck(t, m, key('p')))
	sils, err = st.ListSilences(context.Background(), true)
	if err != nil || len(sils) != 0 {
		t.Fatalf("after resume: silences = %+v (%v), want none", sils, err)
	}
}

// updateCmdNoCheck runs a cmd immediately; it exists because the pause test
// wants the store write to happen before the next assertion.
func updateCmdNoCheck(t *testing.T, m Model, msg tea.Msg) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatalf("key %v produced no command", msg)
	}
	return cmd
}

// TestMuteAndUnmute drives m through the mute modal (mandatory expiry preset)
// into a notify mute and back, and asserts the reason is logged.
func TestMuteAndUnmute(t *testing.T) {
	st := openTestStore(t)
	m := New(st, &Write{})
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())

	m, _ = updateCmd(t, m, key('m'))
	if m.modal != modalMute {
		t.Fatalf("m did not open the mute modal, modal = %v", m.modal)
	}
	for _, r := range "maintenance window" {
		m = update(t, m, key(r))
	}
	m = runCmd(t, m, updateCmdNoCheck(t, m, specialKey(tea.KeyEnter)))
	mutes, err := st.ListNotifyMutes(context.Background(), true)
	if err != nil || len(mutes) != 1 {
		t.Fatalf("after mute: mutes = %+v (%v), want one", mutes, err)
	}
	if mutes[0].Reason != "maintenance window" {
		t.Fatalf("mute reason = %q, want the logged reason", mutes[0].Reason)
	}

	m = update(t, m, loadGlanceCmd(st)())
	m = runCmd(t, m, updateCmdNoCheck(t, m, key('m')))
	mutes, err = st.ListNotifyMutes(context.Background(), true)
	if err != nil || len(mutes) != 0 {
		t.Fatalf("after unmute: mutes = %+v (%v), want none", mutes, err)
	}
}

// TestResumeAndUnmuteLiftOnlyGlobalHolds proves p/m never touch scoped holds
// the web set: resume lifts only global silences and unmute only global notify
// mutes, leaving an alert or host hold in place.
func TestResumeAndUnmuteLiftOnlyGlobalHolds(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	globalSil, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create global silence: %v", err)
	}
	hostSil, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeHost, ScopeValue: "aurora", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create host silence: %v", err)
	}

	runCmd(t, New(st, nil), resumeCmd(st, []store.Silence{globalSil, hostSil}))
	sils, err := st.ListSilences(ctx, true)
	if err != nil || len(sils) != 1 || sils[0].ID != hostSil.ID {
		t.Fatalf("after resume: %+v (%v), want only the host silence left", sils, err)
	}

	globalMute, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create global mute: %v", err)
	}
	alertMute, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeAlert, ScopeValue: "disk-pressure", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create alert mute: %v", err)
	}

	runCmd(t, New(st, nil), unmuteCmd(st, []store.NotifyMute{globalMute, alertMute}))
	mutes, err := st.ListNotifyMutes(ctx, true)
	if err != nil || len(mutes) != 1 || mutes[0].ID != alertMute.ID {
		t.Fatalf("after unmute: %+v (%v), want only the alert mute left", mutes, err)
	}
}

// TestApproveThroughTheReactiveSeam drives a full approve: the envelope's
// capability token, the enqueue-only queue, and the service's decision verb —
// ending with the job released into queued and the approval settled.
func TestApproveThroughTheReactiveSeam(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	approval := seedApproval(t, st, env, job.ID, "", 0, nil)

	m := New(st, w)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, key('5'))

	m = runCmd(t, m, updateCmdNoCheck(t, m, key('a')))
	got, err := st.GetApproval(context.Background(), approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if got.State != store.ApprovalApproved {
		t.Fatalf("approval state = %s, want approved", got.State)
	}
	job2, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job2.State != store.StateQueued {
		t.Fatalf("approved job state = %s, want queued (released, not run)", job2.State)
	}
	if !strings.Contains(m.flash, "approved") {
		t.Fatalf("flash = %q, want the approved notice", m.flash)
	}
}

// TestRejectWithdrawsTheGatedJob drives r through the same seam and asserts
// the job is cancelled, never released.
func TestRejectWithdrawsTheGatedJob(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	seedApproval(t, st, env, job.ID, "", 0, nil)

	m := New(st, w)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, key('5'))
	runCmd(t, m, updateCmdNoCheck(t, m, key('r')))

	job2, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job2.State != store.StateCancelled {
		t.Fatalf("rejected job state = %s, want cancelled", job2.State)
	}
}

// TestApproveStalePlanRefused proves the fail-closed refusal: an approval
// whose workspace moved past its plan's serial shows the stale refusal and
// offers no approve key — a itself produces nothing and the row stays pending.
func TestApproveStalePlanRefused(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	ctx := context.Background()

	ws, err := st.CreateWorkspace(ctx, store.Workspace{Name: "core", TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	job := seedGatedJob(t, st)
	seedApproval(t, st, env, job.ID, "core", 0, nil)
	// The workspace advances past the plan's serial: the mover.
	mover := seedGatedJob(t, st)
	if _, err := st.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 7, JobID: &mover.ID, Ciphertext: []byte("ct"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}

	m := New(st, w)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, key('5'))

	if v := m.View().Content; !strings.Contains(v, "PLAN STALE") || strings.Contains(v, "[a] approve") {
		t.Fatalf("stale approval did not render the refusal (no approve key):\n%s", v)
	}
	m, cmd := updateCmd(t, m, key('a'))
	if cmd != nil {
		t.Fatal("approve on a stale plan produced a command; nothing may be released")
	}
	as, err := st.ListApprovals(ctx, true)
	if err != nil || len(as) != 1 {
		t.Fatalf("pending approvals = %+v (%v), want the one, still pending", as, err)
	}

	// r still rejects the stale plan directly.
	runCmd(t, m, updateCmdNoCheck(t, m, key('r')))
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateCancelled {
		t.Fatalf("rejected stale plan's job = %s, want cancelled", got.State)
	}
}

// TestApproveDestroyCarryingPlanRequiresTypedConfirm proves a destroys plan
// opens the typed-confirmation modal and only an exact workspace name lets the
// approve through.
func TestApproveDestroyCarryingPlanRequiresTypedConfirm(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	art := seedPlanArtifact(t, st, env, job.ID, true)
	approval := seedApproval(t, st, env, job.ID, "core", 1, art)

	m := New(st, w)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, key('5'))

	m = runCmd(t, m, updateCmdNoCheck(t, m, key('a')))
	if m.modal != modalDestroy {
		t.Fatalf("destroy-carrying approve did not open the typed-confirm modal, modal = %v", m.modal)
	}
	// A wrong name is refused.
	for _, r := range "corp" {
		m = update(t, m, key(r))
	}
	m = update(t, m, specialKey(tea.KeyEnter))
	as, _ := st.ListApprovals(context.Background(), true)
	if len(as) != 1 {
		t.Fatalf("a wrong typed name approved the plan: %d pending", len(as))
	}
	// The exact workspace name lets it through.
	m = update(t, m, specialKey(tea.KeyEscape))
	m, _ = updateCmd(t, m, key('a'))
	m = runCmd(t, m, updateCmdNoCheck(t, m, key('a')))
	for _, r := range "core" {
		m = update(t, m, key(r))
	}
	m = runCmd(t, m, updateCmdNoCheck(t, m, specialKey(tea.KeyEnter)))
	got, err := st.GetApproval(context.Background(), approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if got.State != store.ApprovalApproved {
		t.Fatalf("typed-confirmed approval state = %s, want approved", got.State)
	}
}

// TestApprovalDiffRedactsSensitivePlan proves the [d] pager never shows a
// secret value: a plan whose *_sensitive trees mark anything true renders
// [redacted · secret] for the resource and the SECRETS section counts it, the
// shared wire.HasSensitivePlan classifier the web uses. Deleting the
// redaction branch leaves no marker and fails.
func TestApprovalDiffRedactsSensitivePlan(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	art := seedSensitivePlanArtifact(t, st, env, job.ID)
	approval := seedApproval(t, st, env, job.ID, "core", 1, art)

	msg := loadApprovalDiffCmd(st, w, approval)()
	loaded, ok := msg.(diffLoadedMsg)
	if !ok {
		t.Fatalf("loadApprovalDiffCmd() = %T, want diffLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("load diff: %v", loaded.err)
	}
	got := renderApprovalDiff(loaded.model)
	if strings.Contains(got, "s3cr3t-p@ss") {
		t.Fatalf("diff pager leaked the secret value:\n%s", got)
	}
	if !strings.Contains(got, "[redacted · secret]") || !strings.Contains(got, "SECRETS") {
		t.Fatalf("diff pager did not mark the secret resource redacted:\n%s", got)
	}
}

// TestApprovalDiffUsesShowCompanion proves the [d] pager renders the sealed
// `tofu show -json` companion, not the binary saved plan the approval points
// at: the fixture hands the saved plan non-JSON bytes, so only a resolver that
// prefers the companion can produce a diff.
func TestApprovalDiffUsesShowCompanion(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	art := seedPlanArtifact(t, st, env, job.ID, true)
	approval := seedApproval(t, st, env, job.ID, "core", 1, art)

	msg := loadApprovalDiffCmd(st, w, approval)()
	loaded, ok := msg.(diffLoadedMsg)
	if !ok {
		t.Fatalf("loadApprovalDiffCmd() = %T, want diffLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("load diff: %v", loaded.err)
	}
	got := renderApprovalDiff(loaded.model)
	if !strings.Contains(got, "aws_db_instance.legacy_reports") || !strings.Contains(got, "DESTROYS") {
		t.Fatalf("diff pager read the binary saved plan, not the show companion:\n%s", got)
	}
}

// TestDestroyConfirmUsesShowCompanion proves the typed-confirm gate counts
// destroys from the show-json companion: the binary saved plan parses to zero,
// so the modal would silently never fire without the companion.
func TestDestroyConfirmUsesShowCompanion(t *testing.T) {
	st := openTestStore(t)
	w, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	art := seedPlanArtifact(t, st, env, job.ID, true)
	approval := seedApproval(t, st, env, job.ID, "core", 1, art)

	msg := checkDestroyCmd(st, w, approval.ID)()
	got, ok := msg.(destroyCheckMsg)
	if !ok {
		t.Fatalf("checkDestroyCmd() = %T, want destroyCheckMsg", msg)
	}
	if got.err != nil {
		t.Fatalf("check destroy: %v", got.err)
	}
	if got.destroys != 1 || got.resource != "aws_db_instance.legacy_reports" {
		t.Fatalf("destroy summary = %d %q, want 1 aws_db_instance.legacy_reports (the binary saved plan parsed to zero)", got.destroys, got.resource)
	}
}

// TestPlanShowBlobFallsBackWithoutCompanion proves the resolver still parses a
// plan that has no show companion — an operator's own plan file, or one sealed
// before the capture existed. Dropping the fallback leaves the plan unreadable.
func TestPlanShowBlobFallsBackWithoutCompanion(t *testing.T) {
	st := openTestStore(t)
	_, env := newTestWrite(t)
	job := seedGatedJob(t, st)
	artID := seedOperatorPlanArtifact(t, st, env, job.ID)
	art, err := st.GetArtifact(context.Background(), *artID)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}

	blob, err := planShowBlob(context.Background(), env, st, art)
	if err != nil {
		t.Fatalf("planShowBlob: %v", err)
	}
	if n, first := planDestroySummary(blob); n != 1 || first != "aws_db_instance.legacy_reports" {
		t.Fatalf("fallback parsed %d %q, want 1 aws_db_instance.legacy_reports from the plan's own bytes", n, first)
	}
}

// TestTUIEnqueueOnlyNeverDrains is the invariant-5 guard: the queue an
// operator write builds is enqueue-only, so its driver refuses to run a job.
// Making the driver drainable (returning a result) fails this test red.
func TestTUIEnqueueOnlyNeverDrains(t *testing.T) {
	st := openTestStore(t)
	res, err := enqueueOnly{}.Run(context.Background(),
		executor.Spec{Argv: []string{"true"}}, executor.SinkFunc(func(executor.Event) {}))
	if err == nil {
		t.Fatalf("enqueue-only driver ran a job (%+v); a TUI write path must never drain the queue", res)
	}
	_ = st
}

// TestRunNowKeysEnqueue drives b on [2], l on [3], and x on [4] and proves each
// releases exactly one job through the shared spec builders: a backup, a
// TUI-triggered tofu plan, and a schedule's shell spec. It is the run-now
// wiring's end-to-end assertion; dropping a dispatch case, the nil-Write guard,
// or the wrong trigger fails it.
func TestRunNowKeysEnqueue(t *testing.T) {
	st := openTestStore(t)
	w, _ := newTestWrite(t)
	ctx := context.Background()

	target, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "restic-local", Kind: "restic_local", URI: "/var/lib/exocomp/restic", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup target: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "core", TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-shell", CronExpr: "@daily", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"true"}},
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	m := New(st, w)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadBackupsCmd(st)())
	m = update(t, m, loadWorkspacesCmd(st)())
	m = update(t, m, loadSchedulesCmd(st)())

	// [2] b: one backup job for the selected target.
	m = update(t, m, key('2'))
	runCmd(t, m, updateCmdNoCheck(t, m, key('b')))
	jobs := listJobs(t, st)
	if len(jobs) != 1 {
		t.Fatalf("b enqueued %d jobs, want 1", len(jobs))
	}
	if jobs[0].Kind != backup.JobKind || jobs[0].Trigger != service.TriggerTUI || jobs[0].Runbook != "backup" {
		t.Fatalf("b job = {kind:%q trigger:%q runbook:%q}, want backup/tui/backup",
			jobs[0].Kind, jobs[0].Trigger, jobs[0].Runbook)
	}
	if argv := jobs[0].Args.Argv; len(argv) != 2 || argv[1] != target.Name {
		t.Fatalf("b argv = %v, want the selected target %q", argv, target.Name)
	}

	// [3] l: one TUI-triggered tofu plan for the selected workspace.
	m = update(t, m, key('3'))
	runCmd(t, m, updateCmdNoCheck(t, m, key('l')))
	plan := jobOfKind(listJobs(t, st), executor.KindTofu)
	if plan == nil {
		t.Fatal("l enqueued no tofu plan job")
	}
	if plan.Trigger != service.TriggerTUI || plan.Runbook != "workspace:core" {
		t.Fatalf("l job = {trigger:%q runbook:%q}, want tui/workspace:core", plan.Trigger, plan.Runbook)
	}

	// [4] x: one shell job from the schedule's own spec builder.
	m = update(t, m, key('4'))
	runCmd(t, m, updateCmdNoCheck(t, m, key('x')))
	shell := jobOfKind(listJobs(t, st), store.ScheduleShell)
	if shell == nil {
		t.Fatal("x enqueued no schedule job")
	}
	if shell.Trigger != service.TriggerTUI || shell.Runbook != "nightly-shell" {
		t.Fatalf("x job = {trigger:%q runbook:%q}, want tui/nightly-shell", shell.Trigger, shell.Runbook)
	}
}

func listJobs(t *testing.T, st *store.Store) []store.Job {
	t.Helper()
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return jobs
}

func jobOfKind(jobs []store.Job, kind string) *store.Job {
	for i := range jobs {
		if jobs[i].Kind == kind {
			return &jobs[i]
		}
	}
	return nil
}

// TestCancelJobConfirms proves c opens the confirm and cancels nothing until
// enter: esc leaves the job queued, enter presses the transition. The mutation
// (making c cancel directly) reds the "nothing before enter" assertion.
func TestCancelJobConfirms(t *testing.T) {
	st := openTestStore(t)
	job := seedJob(t, st, "ansible", store.StateQueued, nil)
	ctx := context.Background()

	m := New(st, &Write{})
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1'))
	m = update(t, m, loadJobsCmd(st)())

	m, cmd := updateCmd(t, m, key('c'))
	if cmd != nil {
		t.Fatal("c produced a command before the confirm; nothing may be cancelled yet")
	}
	if m.modal != modalCancelJob {
		t.Fatalf("c did not open the cancel confirm, modal = %v", m.modal)
	}
	if got, err := st.GetJob(ctx, job.ID); err != nil || got.State != store.StateQueued {
		t.Fatalf("before enter: job state = %v (%v), want queued", got.State, err)
	}

	// esc keeps the job.
	m = update(t, m, specialKey(tea.KeyEscape))
	if m.modal != modalNone {
		t.Fatalf("esc left the confirm open, modal = %v", m.modal)
	}
	if got, err := st.GetJob(ctx, job.ID); err != nil || got.State != store.StateQueued {
		t.Fatalf("after esc: job state = %v (%v), want queued", got.State, err)
	}

	// c then enter cancels.
	m = update(t, m, key('c'))
	if m.modal != modalCancelJob {
		t.Fatalf("second c did not reopen the confirm, modal = %v", m.modal)
	}
	runCmd(t, m, updateCmdNoCheck(t, m, specialKey(tea.KeyEnter)))
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateCancelled {
		t.Fatalf("after enter: job state = %s, want cancelled", got.State)
	}
}

// TestCancelTerminalJobIsANoOp proves a finished job has nothing to cancel: c
// flashes the refusal, opens no confirm, and the row stays terminal.
func TestCancelTerminalJobIsANoOp(t *testing.T) {
	st := openTestStore(t)
	zero := 0
	seedJob(t, st, "ansible", store.StateSucceeded, &zero)

	m := New(st, &Write{})
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1'))
	m = update(t, m, loadJobsCmd(st)())

	got, cmd := updateCmd(t, m, key('c'))
	if cmd != nil {
		t.Fatal("c on a terminal job produced a command; nothing may be cancelled")
	}
	if got.modal != modalNone {
		t.Fatalf("c on a terminal job opened a confirm, modal = %v", got.modal)
	}
	if !strings.Contains(got.flash, "nothing to cancel") {
		t.Fatalf("flash = %q, want the terminal refusal", got.flash)
	}
}

// TestToggleScheduleFlipsEnabled drives space on the schedules screen and back.
func TestToggleScheduleFlipsEnabled(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "@daily", Kind: store.ScheduleBackup,
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	m := New(st, &Write{})
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('4'))
	m = update(t, m, loadSchedulesCmd(st)())

	m = runCmd(t, m, updateCmdNoCheck(t, m, key(' ')))
	scheds, err := st.ListSchedules(ctx, false)
	if err != nil || len(scheds) != 1 || scheds[0].Enabled {
		t.Fatalf("after space: %+v (%v), want the schedule disabled", scheds, err)
	}

	// The write's reload lands (the runtime does this automatically), then
	// space toggles back on from the fresh row.
	m = update(t, m, loadSchedulesCmd(st)())
	runCmd(t, m, updateCmdNoCheck(t, m, key(' ')))
	scheds, err = st.ListSchedules(ctx, false)
	if err != nil || len(scheds) != 1 || !scheds[0].Enabled {
		t.Fatalf("after second space: %+v (%v), want the schedule enabled", scheds, err)
	}
}

// TestFilterNarrowsTheActiveTable drives / through the filter input and
// proves the jobs table only keeps matching rows.
func TestFilterNarrowsTheActiveTable(t *testing.T) {
	st := openTestStore(t)
	seedJob(t, st, "ansible", store.StateQueued, nil)
	seedJob(t, st, "restic", store.StateQueued, nil)

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1'))
	m = update(t, m, loadJobsCmd(st)())

	m, _ = updateCmd(t, m, key('/'))
	if m.modal != modalFilter {
		t.Fatalf("/ did not open the filter input, modal = %v", m.modal)
	}
	for _, r := range "restic" {
		m = update(t, m, key(r))
	}
	m = update(t, m, specialKey(tea.KeyEnter))
	if m.filter != "restic" {
		t.Fatalf("filter = %q, want restic", m.filter)
	}
	if got := m.View().Content; !strings.Contains(got, "restic") || strings.Contains(got, "ansible") {
		t.Fatalf("filtered jobs view still shows unfiltered rows:\n%s", got)
	}
}

// TestFilterIgnoresColorCodes proves the table filter matches row text, not
// the SGR sequences styling it: a numeric filter (the digits inside a colour
// code) must not match styled rows, and a plain word inside a styled cell
// still does.
func TestFilterIgnoresColorCodes(t *testing.T) {
	st := openTestStore(t)
	seedJob(t, st, "ansible", store.StateSucceeded, nil)

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1'))
	m = update(t, m, loadJobsCmd(st)())

	// A row's STATE cell is styled: "succeeded" still matches once SGR is
	// stripped, but the escape digits (here the 38 inside the colour code) do
	// not.
	m, _ = updateCmd(t, m, key('/'))
	for _, r := range "succeeded" {
		m = update(t, m, key(r))
	}
	m = update(t, m, specialKey(tea.KeyEnter))
	if got := m.View().Content; !strings.Contains(got, "succeeded") {
		t.Fatalf("filter by a styled word dropped the row:\n%s", got)
	}

	// The modal pre-fills the previous filter, so reset it before typing a
	// fresh numeric filter. Filter on "38;2" — the true-colour SGR signature
	// (the semicolon can only come from the colour code, never from row data
	// like an ID or a queued time, which is what makes the old bare "38"
	// flaky: a wall-clock minute/second containing "38" matched and kept the
	// row). After SGR stripping there is no "38;2" left, so zero rows match.
	m.filter = ""
	m, _ = updateCmd(t, m, key('/'))
	for _, r := range "38;2" {
		m = update(t, m, key(r))
	}
	m = update(t, m, specialKey(tea.KeyEnter))
	if len(m.jobsTable.model.Rows()) != 0 {
		t.Fatalf("numeric filter matched a colour code; %d rows kept, want none", len(m.jobsTable.model.Rows()))
	}
}

// TestEventsOutcomeFilterAndTrace drives o through the outcome cycle and e
// into the trace overlay for the selected event.
func TestEventsOutcomeFilterAndTrace(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, ev := range []store.Event{
		{Source: "aurora", ReceivedAt: now.Add(-2 * time.Minute), Normalized: "{}", Raw: "{}",
			MatchedRule: "disk-pressure", Outcome: store.OutcomeFired, Reason: "ran disk-clean"},
		{Source: "aurora", ReceivedAt: now.Add(-3 * time.Minute), Normalized: "{}", Raw: "{}",
			MatchedRule: "disk-pressure", Outcome: store.OutcomeSuppressed, Reason: "silence maintenance"},
	} {
		if _, err := st.CreateEvent(ctx, ev); err != nil {
			t.Fatalf("create event: %v", err)
		}
	}

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, key('6'))

	m, _ = updateCmd(t, m, key('o'))
	if m.eventOutcome != store.OutcomeIgnored {
		t.Fatalf("first o = %s, want the first outcome", m.eventOutcome)
	}
	if got := m.View().Content; strings.Contains(got, "FIRED") || strings.Contains(got, "SUPPRESSED") {
		t.Fatalf("events view with outcome filter shows every event:\n%s", got)
	}

	m, _ = updateCmd(t, m, key('e'))
	if !m.traceOpen {
		t.Fatal("e did not open the trace overlay")
	}
	if got := m.View().Content; !strings.Contains(got, "trace ·") {
		t.Fatalf("trace overlay missing its title:\n%s", got)
	}
	m = update(t, m, key('x')) // any key closes the trace
	if m.traceOpen {
		t.Fatal("a key did not close the trace overlay")
	}
}

// TestHelpOverlayShowsTheKeymap drives ? to the full keymap and back, and
// proves the run-now keys moved out of the deferred block into the live map.
func TestHelpOverlayShowsTheKeymap(t *testing.T) {
	m := New(openTestStore(t), nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m, _ = updateCmd(t, m, key('?'))
	if !m.helpOpen {
		t.Fatal("? did not open the help overlay")
	}
	got := m.View().Content
	if !strings.Contains(got, "exocomp keys") {
		t.Fatalf("help overlay missing the keymap:\n%s", got)
	}
	for _, want := range []string{"b run now", "l plan now", "x run now"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help overlay missing the live run-now key %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "deferred") {
		t.Fatalf("help overlay still carries the deferred block:\n%s", got)
	}
	m = update(t, m, key('x'))
	if m.helpOpen {
		t.Fatal("a key did not close the help overlay")
	}
}

// TestRunNowHelpBarsNameTheKeys proves each screen's help bar names its now-live
// run-now key.
func TestRunNowHelpBarsNameTheKeys(t *testing.T) {
	m := New(openTestStore(t), nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = update(t, m, key('2'))
	if got := m.renderHelp(); !strings.Contains(got, "[b] run now") {
		t.Fatalf("backups help bar = %q, want [b] run now", got)
	}
	m = update(t, m, key('3'))
	if got := m.renderHelp(); !strings.Contains(got, "[l] plan now") {
		t.Fatalf("workspaces help bar = %q, want [l] plan now", got)
	}
	m = update(t, m, key('4'))
	if got := m.renderHelp(); !strings.Contains(got, "[x] run now") {
		t.Fatalf("schedules help bar = %q, want [x] run now", got)
	}
}

// TestGAndGMoveTheCursorToTopAndBottom proves g/G on the jobs table.
func TestGAndGMoveTheCursorToTopAndBottom(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 3; i++ {
		seedJob(t, st, "ansible", store.StateQueued, nil)
	}
	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1'))
	m = update(t, m, loadJobsCmd(st)())

	m = update(t, m, specialKey(tea.KeyDown))
	if m.jobsTable.model.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1 after one down", m.jobsTable.model.Cursor())
	}
	m = update(t, m, key('G'))
	if m.jobsTable.model.Cursor() != 2 {
		t.Fatalf("cursor = %d after G, want the last row", m.jobsTable.model.Cursor())
	}
	m = update(t, m, key('g'))
	if m.jobsTable.model.Cursor() != 0 {
		t.Fatalf("cursor = %d after g, want the first row", m.jobsTable.model.Cursor())
	}
}
