package queue

import (
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestNonDecision pins the resume distinction the sequencer turns on: a step
// whose job did not reach a decision is re-run, one that did is settled. The
// subtle case is cancelled — a shutdown that killed a RUNNING step re-runs,
// while an approval the gate denied (cancelled before it ever started) does
// not, so a denied step blocks the workflow instead of looping forever.
func TestNonDecision(t *testing.T) {
	started := time.Now().UTC()
	cases := []struct {
		name string
		job  store.Job
		want bool
	}{
		{"interrupted crash", store.Job{State: store.StateInterrupted}, true},
		{"shutdown killed a running step",
			store.Job{State: store.StateCancelled, StartedAt: &started}, true},
		{"approval denied before it ran",
			store.Job{State: store.StateCancelled, StartedAt: nil}, false},
		{"failed on its own exit code", store.Job{State: store.StateFailed}, false},
		{"timed out", store.Job{State: store.StateTimedOut}, false},
		{"succeeded", store.Job{State: store.StateSucceeded}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nonDecision(tc.job); got != tc.want {
				t.Errorf("nonDecision(%s, started=%v) = %v, want %v",
					tc.job.State, tc.job.StartedAt != nil, got, tc.want)
			}
		})
	}
}
