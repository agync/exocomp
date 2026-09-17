package web

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// The forward-only migration series is the single largest cost of building a
// harness, far more than the keyring or the queue. The schema is identical for
// every test, so it is built once per test binary into a template file and
// copied into each test's temp dir. Each harness still owns its file, store,
// keyring, and data dir; only the pre-migrated schema bytes are shared.
var (
	templateOnce sync.Once
	templateDir  string
	templateData []byte
	templateErr  error
)

func templateDB() ([]byte, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "exocomp-web-template-")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		st, err := store.Open(filepath.Join(dir, "exocomp.db"))
		if err != nil {
			templateErr = err
			return
		}
		if err := st.Close(); err != nil {
			templateErr = err
			return
		}
		templateData, templateErr = os.ReadFile(filepath.Join(dir, "exocomp.db"))
	})
	return templateData, templateErr
}

// TestMain removes the template dir the lazy initializer created; in -short
// mode no harness runs, so no template is ever built.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// parallelize marks t parallel the first time a harness is built for it.
// testing.T panics on a repeated t.Parallel, and a few tests build two
// harnesses on one *testing.T; the set lets those share one parallel slot.
var parallelized sync.Map

func parallelize(t *testing.T) {
	t.Helper()
	if _, loaded := parallelized.LoadOrStore(t, struct{}{}); !loaded {
		t.Parallel()
	}
}

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
func newHarness(t *testing.T) *harness { return newHarnessBase(t, "") }

// newHarnessSerial builds the same surface without marking the test parallel.
// A test that mutates a package global read by a live handler (version.Version,
// version.Commit on /system) must not overlap another test's server, so it
// takes the sequential slot by foregoing t.Parallel.
func newHarnessSerial(t *testing.T) *harness { return buildHarness(t, "", false) }

// newHarnessBase builds the same surface under a base path: "" is the origin
// root, "/exocomp" the prefixed origin. It exists so the base-path tests drive
// the identical wiring `exocomp web --base-path /exocomp` builds.
func newHarnessBase(t *testing.T, base string) *harness { return buildHarness(t, base, true) }

func buildHarness(t *testing.T, base string, parallel bool) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("web harness integration")
	}
	if parallel {
		parallelize(t)
	}
	dir := t.TempDir()

	db, err := templateDB()
	if err != nil {
		t.Fatalf("harness template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exocomp.db"), db, 0o600); err != nil {
		t.Fatalf("seed harness store: %v", err)
	}

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
		Version: "test", DataDir: dir, BasePath: base,
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
