package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// runDevRun and devRunSpec were the early scaffolding command `exocomp devrun --
// <argv...>`: an ungoverned, unsandboxed shell-argv verb with no reason to
// ship (the nonfunctional review that removed it). The mechanism underneath —
// open the store through the sweep, enqueue one shell job, drain it in this
// process, print the outcome — is still what a handful of invariant tests
// need to drive a real job through the real queue without a running daemon,
// so it lives on here, test-only: nothing in this file is compiled into the
// shipped binary, and no command wires it to a caller's input.

type devRunSpec struct {
	argv           []string
	timeout        time.Duration
	concurrencyKey string
	// idempotencyKey dedups a retried job to one row; empty on the unchanged path.
	idempotencyKey string
}

func runDevRun(ctx context.Context, w io.Writer, spec devRunSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	st, err := openJobStore(ctx, w)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	})
	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:        "devrun",
		Kind:           "shell",
		Trigger:        "manual",
		Argv:           spec.argv,
		ConcurrencyKey: spec.concurrencyKey,
		IdempotencyKey: spec.idempotencyKey,
		Timeout:        spec.timeout,
	})
	if err != nil {
		return err
	}
	if err := q.Drain(ctx); err != nil {
		return err
	}

	final, err := st.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	events, err := st.LastJobEvents(ctx, job.ID, 5)
	if err != nil {
		return err
	}
	printJob(w, final, events)

	if final.State != store.StateSucceeded {
		return fmt.Errorf("devrun: job %d ended %s", final.ID, final.State)
	}
	return nil
}

func printJob(w io.Writer, job store.Job, events []store.JobEvent) {
	exit := "-"
	if job.ExitCode != nil {
		exit = fmt.Sprintf("%d", *job.ExitCode)
	}
	_, _ = fmt.Fprintf(w, "job %d  state: %s  exit: %s  duration: %s\n",
		job.ID, job.State, exit, jobDuration(job))

	_, _ = fmt.Fprintf(w, "last %d event(s):\n", len(events))
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "  %d [%s] %s\n", e.Seq, e.Level, e.Msg)
	}
}

func TestRunDevRunReportsFinalState(t *testing.T) {
	useDataDir(t, t.TempDir())

	var out bytes.Buffer
	err := runDevRun(context.Background(), &out, devRunSpec{argv: []string{"/bin/echo", "hello"}})
	if err != nil {
		t.Fatalf("devrun: %v", err)
	}

	got := out.String()
	for _, want := range []string{"state: succeeded", "exit: 0", "[info] hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
}

func TestRunDevRunFailsWithTheJob(t *testing.T) {
	useDataDir(t, t.TempDir())

	var out bytes.Buffer
	err := runDevRun(context.Background(), &out, devRunSpec{
		argv:    []string{"/bin/sleep", "30"},
		timeout: 150 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("devrun returned nil for a timed-out job")
	}
	if !strings.Contains(out.String(), "state: timed_out") {
		t.Errorf("output = %q", out.String())
	}
}
