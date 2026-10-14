package health

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestCountPrivileged is the pure tally gate: jobs arrive newest-first, the
// scan stops at the first row older than the window, and only rows carrying the
// unsandboxed marker count. A break in the window boundary or the marker check
// must red.
func TestCountPrivileged(t *testing.T) {
	now := time.Now()
	window := now.Add(-PrivilegePostureWindow)
	jobs := []store.Job{
		{QueuedAt: now.Add(-1 * time.Hour), Args: store.JobArgs{Privileged: true}},
		{QueuedAt: now.Add(-2 * time.Hour)},
		{QueuedAt: now.Add(-48 * time.Hour), Args: store.JobArgs{Privileged: true}},
		// Older than the window: outside the tally, and the scan stops here.
		{QueuedAt: window.Add(-1 * time.Hour), Args: store.JobArgs{Privileged: true}},
		{QueuedAt: window.Add(-2 * time.Hour), Args: store.JobArgs{Privileged: true}},
	}
	privileged, total := countPrivileged(jobs, window)
	if privileged != 2 || total != 3 {
		t.Errorf("countPrivileged = (%d, %d), want (2, 3)", privileged, total)
	}
}

// TestPrivilegePostureEntry classifies the tally: any unsandboxed run in the
// window is a warn, none is the quiet ok state, and no database is unknown —
// a state to read, not a fault.
func TestPrivilegePostureEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  PrivilegePostureProbe
		want string
	}{
		{"none", PrivilegePostureProbe{Observed: true, Total: 12}, wire.HealthOK},
		{"some", PrivilegePostureProbe{Observed: true, Privileged: 2, Total: 12}, wire.HealthWarn},
		{"no db", PrivilegePostureProbe{}, wire.HealthUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privilegePostureEntry(tc.res)
			if got.Name != "privilege_posture" {
				t.Errorf("entry name = %q, want privilege_posture", got.Name)
			}
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if got.Detail == "" {
				t.Error("entry carries no detail")
			}
		})
	}
}

// TestProbePrivilegePostureCounts reads the real store: a privileged and a
// sandboxed job queued now tally 1 of 2, and an absent database is Observed
// false rather than a fabricated zero.
func TestProbePrivilegePostureCounts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	for _, privileged := range []bool{true, false} {
		if _, err := st.CreateJob(ctx, store.NewJob{
			Runbook: "playbook", Kind: "ansible", Trigger: "manual",
			Args: store.JobArgs{Argv: []string{"/bin/true"}, Privileged: privileged},
		}); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	open := func() (Store, func() error, error) { return st, func() error { return nil }, nil }

	res := ProbePrivilegePosture(ctx, dir, open)
	if !res.Observed || res.Privileged != 1 || res.Total != 2 {
		t.Errorf("ProbePrivilegePosture = %+v, want observed 1 of 2", res)
	}

	empty := ProbePrivilegePosture(ctx, t.TempDir(), open)
	if empty.Observed {
		t.Errorf("absent database must not be observed: %+v", empty)
	}
}
