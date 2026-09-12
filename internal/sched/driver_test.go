package sched

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
)

// recordingDriver records every kind it was asked to run, so a test can assert
// the router sent a kind to the right underlying driver without a real job.
type recordingDriver struct {
	kinds  []string
	result executor.Result
}

func (d *recordingDriver) Run(_ context.Context, spec executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.kinds = append(d.kinds, spec.Kind)
	return d.result, nil
}

// TestDriverRoutesKinds pins the router table: shell jobs to the shell
// driver; backup, drill, and forget jobs to the backup Runner underneath —
// Runner.Run itself tells the three kinds apart.
func TestDriverRoutesKinds(t *testing.T) {
	t.Parallel()
	shell := &recordingDriver{result: executor.Result{ExitCode: 7}}
	bkup := &recordingDriver{result: executor.Result{ExitCode: 3}}
	d := NewDriver(shell, bkup)
	noop := executor.SinkFunc(func(executor.Event) {})

	for _, kind := range []string{KindShell, KindBackup, backup.DrillJobKind, backup.ForgetJobKind} {
		res, err := d.Run(t.Context(), executor.Spec{Kind: kind}, noop)
		if err != nil {
			t.Fatalf("Run kind %q: %v", kind, err)
		}
		if kind == KindShell && res.ExitCode != 7 {
			t.Errorf("shell kind exit = %d, want the shell driver's 7", res.ExitCode)
		}
		if kind != KindShell && res.ExitCode != 3 {
			t.Errorf("kind %q exit = %d, want the backup driver's 3", kind, res.ExitCode)
		}
	}
	if len(shell.kinds) != 1 || shell.kinds[0] != KindShell {
		t.Errorf("shell driver saw %v, want only %q", shell.kinds, KindShell)
	}
	for _, kind := range []string{KindBackup, backup.DrillJobKind, backup.ForgetJobKind} {
		found := false
		for _, k := range bkup.kinds {
			found = found || k == kind
		}
		if !found {
			t.Errorf("backup driver never saw kind %q: %v", kind, bkup.kinds)
		}
	}
}

// TestDriverRefusesUnknownKind: a kind with no slot answers the sched refusal
// sentence instead of being dropped silently.
func TestDriverRefusesUnknownKind(t *testing.T) {
	t.Parallel()
	d := NewDriver(&recordingDriver{}, &recordingDriver{})
	_, err := d.Run(t.Context(), executor.Spec{Kind: "nope"}, executor.SinkFunc(func(executor.Event) {}))
	if err == nil {
		t.Fatal("unknown kind: want error")
	}
	if !strings.Contains(err.Error(), "no driver for kind") {
		t.Errorf("error does not name the missing driver: %v", err)
	}
}
