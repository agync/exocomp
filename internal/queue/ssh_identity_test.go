package queue_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestEnqueueCarriesSSHIdentityFiles proves Spec.SSHIdentityFiles reaches the
// stored job row: the seam a `playbook run --ssh-identity` declaration depends
// on to reach the sandbox profile when whichever process claims the job runs
// it. Empty is the default, unaffected path most jobs take. Dropping the field
// from the Enqueue JobArgs literal fails this test.
func TestEnqueueCarriesSSHIdentityFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.SSHIdentityFiles = []string{"/keys/lima"}
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Errorf("stored SSHIdentityFiles = %v, want [/keys/lima]", got.Args.SSHIdentityFiles)
	}
}

// TestEnqueueGatedCarriesSSHIdentityFiles is EnqueueGated's half of the hop: a
// --require-approval rule's playbook job must keep its identity declaration
// once released, so the gate cannot silently drop the grant. Dropping the
// field from the gate.go JobArgs literal fails this test.
func TestEnqueueGatedCarriesSSHIdentityFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.SSHIdentityFiles = []string{"/keys/lima"}
	gated, err := q.EnqueueGated(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}

	got, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Errorf("stored SSHIdentityFiles = %v, want [/keys/lima]", got.Args.SSHIdentityFiles)
	}
}

// TestExecuteCarriesSSHIdentityFilesToDriver proves the files persisted on the
// row reach the executor's Spec, the field the sandboxed ansible driver reads
// to widen the profile. Dropping the field from the execute mapping fails this
// test.
func TestExecuteCarriesSSHIdentityFilesToDriver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	drv := &recordingDriver{}
	q := queue.New(st, drv, queue.Options{
		Workers:      1,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	spec := shellSpec("/bin/true")
	spec.SSHIdentityFiles = []string{"/keys/lima"}
	if _, err := q.Enqueue(ctx, spec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	if !slices.Equal(drv.last.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Errorf("driver Spec.SSHIdentityFiles = %v, want [/keys/lima]", drv.last.SSHIdentityFiles)
	}
}
