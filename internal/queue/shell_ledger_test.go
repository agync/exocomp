package queue_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// A shell job runs with no kernel sandbox, so its ledger says so at execution
// on every path, as a privileged job's does. A privileged shell job carries
// the privileged line alone, and another kind carries neither.
func TestShellJobLedgerSaysUnsandboxed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		kind       string
		privileged bool
		want       []string
	}{
		{"shell", "shell", false, []string{"running UNSANDBOXED (shell): no kernel sandbox for this job"}},
		{"privileged shell", "shell", true, []string{"running UNSANDBOXED (privileged): no kernel sandbox for this job"}},
		{"backup", "backup", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			q := queue.New(st, &recordingDriver{}, queue.Options{
				Workers:      1,
				PollInterval: 10 * time.Millisecond,
				Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			spec := shellSpec("/bin/true")
			spec.Kind, spec.Privileged = tc.kind, tc.privileged
			job, err := q.Enqueue(ctx, spec)
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			drain(t, q)

			events, err := st.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			var got []string
			for _, e := range events {
				if strings.Contains(e.Msg, "UNSANDBOXED") {
					got = append(got, e.Msg)
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("UNSANDBOXED lines = %q, want %q", got, tc.want)
			}
		})
	}
}
