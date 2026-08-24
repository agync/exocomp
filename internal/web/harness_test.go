package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// harness is a running web surface backed by a real store, envelope, queue, and
// reactive engine — the same wiring `exocomp web` builds, so a test drives the
// production path, not a mock of it.
type harness struct {
	dir    string
	store  *store.Store
	env    *secrets.Envelope
	queue  *queue.Queue
	engine *reactive.Engine
	server *Server
}

// newHarness builds the surface with the queue's workers running, so a released
// job actually runs. The queue stops when the test's context is cancelled.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	env, _, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{Workers: 2, PollInterval: 20 * time.Millisecond})
	engine, err := reactive.New(reactive.Options{
		Store: st, Queue: q, Deriver: env, Notifier: notify.New(nil),
	})
	if err != nil {
		t.Fatalf("reactive.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	srv, err := New(Options{
		Store: st, Deriver: env, Decider: engine, Plans: env, Sealer: env,
		Version: "test", DataDir: dir,
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	return &harness{dir: dir, store: st, env: env, queue: q, engine: engine, server: srv}
}

// mint issues a token of the given scope and returns the one-time string.
func (h *harness) mint(t *testing.T, name, scope string) string {
	t.Helper()
	token, _, err := MintToken(context.Background(), h.store, name, scope, 0)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	return token
}

// gatedJob writes a job straight into awaiting_approval with an approval bound
// to it, the shape the reactive gate produces. argv decides what running it
// does; the default succeeds.
func (h *harness) gatedJob(t *testing.T, ttl time.Duration, argv ...string) (store.Job, store.Approval) {
	t.Helper()
	ctx := context.Background()
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-c", "echo ran; exit 0"}
	}
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: argv,
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	_, hash, err := reactive.ApprovalToken(h.env, job.ID)
	if err != nil {
		t.Fatalf("ApprovalToken: %v", err)
	}
	now := time.Now().UTC()
	approval, err := h.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, TokenHash: hash, RequestedAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return job, approval
}

// waitJobState polls until a job reaches want or the deadline passes.
func (h *harness) waitJobState(t *testing.T, id int64, want store.JobState, within time.Duration) store.Job {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		job, err := h.store.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.State == want {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d is %s after %s, want %s", id, job.State, within, want)
		}
		time.Sleep(15 * time.Millisecond)
	}
}
