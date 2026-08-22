package queue

import (
	"context"
	"errors"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func TestOutcome(t *testing.T) {
	t.Parallel()

	startFailure := errors.New("executor: start /bin/sleep: context canceled")

	for _, tc := range []struct {
		name   string
		res    executor.Result
		runErr error
		ctxErr error
		want   store.JobState
	}{
		{"exit zero", executor.Result{ExitCode: 0}, nil, nil, store.StateSucceeded},
		{"exit non-zero", executor.Result{ExitCode: 2}, nil, nil, store.StateFailed},
		{"driver error", executor.Result{}, errors.New("boom"), nil, store.StateFailed},
		{
			"deadline",
			executor.Result{ExitCode: -1, Signaled: true, TimedOut: true},
			nil, context.DeadlineExceeded, store.StateTimedOut,
		},
		{
			"killed by cancel",
			executor.Result{ExitCode: -1, Signaled: true},
			nil, context.Canceled, store.StateCancelled,
		},
		{
			"cancelled before start",
			executor.Result{}, startFailure, context.Canceled, store.StateCancelled,
		},
		{
			"exited before the cancel landed",
			executor.Result{ExitCode: 3},
			nil, context.Canceled, store.StateFailed,
		},
		{
			// A tolerated degradation settles succeeded, not failed, even though
			// the process exited non-zero — so it does not page.
			"degraded settles succeeded",
			executor.Result{ExitCode: 4, Degraded: true, UnreachableHosts: []string{"laptop"}},
			nil, nil, store.StateSucceeded,
		},
		{
			// A timeout or a driver error wins over Degraded: the tolerance only
			// reclassifies a process that ran to completion on its own.
			"timeout beats degraded",
			executor.Result{ExitCode: -1, Signaled: true, TimedOut: true, Degraded: true},
			nil, context.DeadlineExceeded, store.StateTimedOut,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, msg := outcome(tc.res, tc.runErr, tc.ctxErr)
			if got != tc.want {
				t.Errorf("state = %s, want %s (msg %q)", got, tc.want, msg)
			}
			if msg == "" {
				t.Error("empty outcome message")
			}
		})
	}
}
