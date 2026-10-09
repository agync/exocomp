package main

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// jobContextSink collects a job's emitted lines so a test can read the child's
// environment through its own output.
type jobContextSink struct{ msgs []string }

func (s *jobContextSink) Emit(e executor.Event) { s.msgs = append(s.msgs, e.Msg) }

// TestExecutorSetsJobContextMarker proves the CLI's job-context guard rests on
// a real marker: the executor injects EXOCOMP_JOB_ID for a job (Spec.JobID > 0)
// and never invents one for a non-job call. The CLI constant and the executor's
// must stay the same name.
func TestExecutorSetsJobContextMarker(t *testing.T) {
	if jobContextEnv != executor.JobIDEnv {
		t.Fatalf("CLI job marker %q != executor marker %q", jobContextEnv, executor.JobIDEnv)
	}
	// Seed an inherited value so the zero-job case is distinguishable from a
	// genuinely absent marker.
	t.Setenv(executor.JobIDEnv, "parent")

	for _, tc := range []struct {
		name  string
		jobID int64
		want  string
	}{
		{"not a job", 0, "parent"},
		{"a job", 7, "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink jobContextSink
			if _, err := executor.NewShell().Run(context.Background(), executor.Spec{
				JobID: tc.jobID,
				Argv:  []string{"/bin/sh", "-c", "echo " + executor.JobIDEnv + "=$" + executor.JobIDEnv},
			}, &sink); err != nil {
				t.Fatalf("run: %v", err)
			}
			want := executor.JobIDEnv + "=" + tc.want
			if len(sink.msgs) != 1 || sink.msgs[0] != want {
				t.Fatalf("child env = %q, want %q", sink.msgs, want)
			}
		})
	}
}
