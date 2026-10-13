package wire

import (
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func TestExtractJobProgress(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-10 * time.Second)

	job := store.Job{
		ID:        42,
		State:     store.StateRunning,
		StartedAt: &start,
	}

	events := []store.JobEvent{
		{Seq: 1, Level: "info", Msg: "PLAY [Deploy Web] **************************************"},
		{Seq: 2, Level: "info", Msg: "TASK [Gathering Facts] *********************************"},
		{Seq: 3, Level: "info", Msg: "ok: [web1]"},
		{Seq: 4, Level: "info", Msg: "TASK [nginx : install package] *************************"},
		{Seq: 5, Level: "info", Msg: "changed: [web1]"},
	}

	p := ExtractJobProgress(job, events)
	if p == nil {
		t.Fatal("expected non-nil progress")
	}
	if p.Play != "Deploy Web" {
		t.Errorf("play = %q, want 'Deploy Web'", p.Play)
	}
	if p.Task != "nginx : install package" {
		t.Errorf("task = %q, want 'nginx : install package'", p.Task)
	}
	if p.ElapsedSeconds < 9.0 || p.ElapsedSeconds > 15.0 {
		t.Errorf("elapsed = %v, want ~10.0", p.ElapsedSeconds)
	}
}

func TestExtractJobProgress_NotRunning(t *testing.T) {
	start := time.Now().Add(-10 * time.Second)
	events := []store.JobEvent{
		{Seq: 1, Level: "info", Msg: "PLAY [Build] ******************************************"},
		{Seq: 2, Level: "info", Msg: "TASK [compile] ****************************************"},
	}

	states := []store.JobState{
		store.StateQueued,
		store.StateSucceeded,
		store.StateFailed,
		store.StateCancelled,
		store.StateAwaitingApproval,
	}

	for _, st := range states {
		job := store.Job{
			ID:        42,
			State:     st,
			StartedAt: &start,
		}
		if p := ExtractJobProgress(job, events); p != nil {
			t.Errorf("expected nil progress for state %v, got %+v", st, p)
		}
	}
}
