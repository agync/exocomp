package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// atClock pins the registry's notion of now so ages are exact in the test.
func atClock(r *Registry, now time.Time) { r.now = func() time.Time { return now } }

func TestBackupStalenessGauge(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 14, 3, 0, 0, time.UTC)

	// A leg that succeeded two hours ago.
	fresh, err := st.CreateBackupTarget(ctx, store.BackupTarget{Name: "nightly-db", Kind: "restic_local", URI: "/x", Enabled: true})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run, err := st.StartBackupRun(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	fin := now.Add(-2 * time.Hour)
	run.State = store.BackupSucceeded
	run.FinishedAt = &fin
	if err := st.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	// A leg that has never succeeded.
	if _, err := st.CreateBackupTarget(ctx, store.BackupTarget{Name: "media-store", Kind: "restic_s3", URI: "/y", Enabled: true}); err != nil {
		t.Fatalf("create second target: %v", err)
	}

	r := New(st, "")
	atClock(r, now)
	out, err := r.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	wants := []string{
		"# TYPE exocomp_backup_staleness_seconds gauge",
		`exocomp_backup_staleness_seconds{target="nightly-db"} 7200`,
		`exocomp_backup_staleness_seconds{target="media-store"} +Inf`,
		`exocomp_backup_last_success_timestamp_seconds{target="nightly-db"}`,
		"exocomp_up 1",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("scrape missing %q\n---\n%s", w, out)
		}
	}
	// media-store never succeeded, so it must not emit a last-success timestamp.
	if strings.Contains(out, `exocomp_backup_last_success_timestamp_seconds{target="media-store"}`) {
		t.Errorf("never-succeeded leg emitted a last-success timestamp\n%s", out)
	}
}

func TestQueueAndApprovalGauges(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// A job queued five minutes ago.
	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "rb", Kind: "backup", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	// An approval raised ten minutes ago.
	if _, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, Workspace: "core-infra", RequestedAt: now.Add(-10 * time.Minute),
		ExpiresAt: now.Add(time.Hour), TokenHash: "h", State: store.ApprovalPending,
	}); err != nil {
		t.Fatalf("create approval: %v", err)
	}

	r := New(st, "")
	atClock(r, now.Add(5*time.Minute))
	out, err := r.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, w := range []string{
		"exocomp_queue_oldest_job_age_seconds",
		"exocomp_pending_approvals 1",
		"exocomp_pending_approval_oldest_age_seconds 900",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape missing %q\n---\n%s", w, out)
		}
	}
}

// The two banner states surface as gauges, and they are independent: a mute
// must not set the paused gauge, and a global silence must not set the muted
// gauge.
func TestPausedAndMutedGaugesAreIndependent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create mute: %v", err)
	}

	r := New(st, "")
	out, err := r.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "exocomp_notify_muted 1") {
		t.Errorf("muted gauge not set with an active mute\n%s", out)
	}
	if !strings.Contains(out, "exocomp_reactive_paused 0") {
		t.Errorf("paused gauge set by a mute — the two must be independent\n%s", out)
	}
}

func TestHandlerServesText(t *testing.T) {
	st := testStore(t)
	r := New(st, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "exocomp_up 1") {
		t.Fatalf("body missing exocomp_up\n%s", rec.Body.String())
	}
}
