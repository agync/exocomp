package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// TestDaemonRoutesDoctorKindToTheSelfCheck closes the A1 routing seam: the
// daemon's kind router must send a kind=doctor job to the doctor driver, not
// fall through to the scheduler's shell/backup routing (the same gap the R1
// cli lane closed for executor.KindCLI). It drives serveDriver in-process —
// no binary build — over a real envelope and store, enqueues one self-check,
// and asserts the run produced the doctor driver's finding events rather than
// a "no driver for kind" failure.
func TestDaemonRoutesDoctorKindToTheSelfCheck(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()
	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	drv, err := serveDriver(dir, st, env, srv)
	if err != nil {
		t.Fatalf("serveDriver: %v", err)
	}
	q := queue.New(st, drv, queue.Options{Workers: 1, PollInterval: 10 * time.Millisecond})

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook: health.JobRunbook, Kind: health.JobKind, Trigger: "manual",
		Argv: health.Argv(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	done, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !done.State.Terminal() {
		t.Fatalf("doctor job = %s, want a terminal state", done.State)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	text := make([]string, len(events))
	n := 0
	for i, e := range events {
		text[i] = e.Msg
		if e.RawJSON == "" {
			continue
		}
		var f struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &f); err == nil && f.Name != "" {
			n++
		}
	}
	ledger := strings.Join(text, "\n")
	if strings.Contains(ledger, "no driver for kind") {
		t.Fatalf("the daemon's kind router refused the doctor job:\n%s", ledger)
	}
	if n == 0 {
		t.Fatal("the daemon's doctor run wrote no finding events")
	}
}
