// Package service holds the transport-agnostic write verbs the CLI, and every
// future programmatic face (HTTP API, MCP), call to change state. Centralizing
// the queue.Spec assembly and the approval-gate routing above one seam is what
// makes every face reach execution through the same path, so no client can have
// a divergent one (AGENTS.md invariant 5).
//
// A verb takes already-parsed, transport-free input and returns a structured
// result: it imports queue, store, reactive, executor, and secrets, never
// net/http, cobra, or any presentation package. Sink-time redaction and the gate
// defaults living in the queue are inherited by everything here.
package service

import (
	"context"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// workspaceRunbookPrefix labels a queued tofu job with the workspace it belongs
// to; it is the only record of that binding, so the daemon reads it back to
// rebuild the job's driver. The CLI keeps its own copy of the literal — the two
// must agree, the same way the daemon already agrees with it.
const workspaceRunbookPrefix = "workspace:"

// WorkspaceRunbook is the runbook name a workspace's plan and apply jobs carry.
func WorkspaceRunbook(name string) string { return workspaceRunbookPrefix + name }

// workspaceConcurrencyKey serializes every tofu run for one workspace, so a
// plan and an apply never race for the same state.
func workspaceConcurrencyKey(name string) string { return "tofu:" + name }

// Enqueuer is how a face turns a queue.Spec into a job. The no-daemon CLI
// implements it by running the job in-process to a terminal state; a face backed
// by a running daemon implements it by queuing the row and returning at once.
// The spec assembly and the gate decision above this interface are shared, so
// the choice of Enqueuer is the only thing that differs between faces.
//
// EnqueueGated is always an enqueue-only insert: a gated job lands in
// awaiting_approval and is never driven here, whichever Enqueuer routes it.
type Enqueuer interface {
	Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error)
	EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error)
}

// Service is the operational core. Its dependencies are the store, the data
// directory the run's artifacts live under, and a clock; the per-call queue
// (as an Enqueuer), envelope, and reactive engine are passed to the verbs that
// need them, because those vary with the face and the run mode.
type Service struct {
	store   *store.Store
	dataDir string
	now     func() time.Time
}

// New returns a Service. A nil clock defaults to time.Now in UTC.
func New(st *store.Store, dataDir string, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: st, dataDir: dataDir, now: now}
}
