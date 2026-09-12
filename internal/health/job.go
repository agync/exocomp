// Package health is the doctor job driver's home: the daemon side of the web
// Doctor verb. The driver runs the one self-check probe (health.Probe) in-process
// and emits one finding event per subsystem entry. The web Doctor verb enqueues a
// kind-doctor job through the one queue (AGENTS.md invariant 5); the daemon's
// kind router sends it here; `exocomp doctor` stays the scriptable twin that
// calls the same Probe directly. A finding's structured shape travels as the
// event's RawJSON so the job page can render findings as rows, and the exit
// code is decided by the worst status — invariant 3: subprocess correctness
// is exit-code truth.
package health

import (
	"context"
	"encoding/json"
	"fmt"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/wire"
)

// JobKind is the queue/executor kind a doctor self-check job carries.
const JobKind = "doctor"

// JobRunbook is the runbook a doctor self-check job is enqueued with — the
// string the jobs list and the /system identity strip filter on, so a lookup
// can never pick up a non-self-check job.
const JobRunbook = "doctor"

// Argv is the doctor job's argv, mirroring the CLI invocation `exocomp doctor
// --output json`. The driver runs the probe in-process and never executes it,
// but the job row carries it so every job face reads the self-check command
// exactly as the CLI names it.
func Argv() []string {
	return []string{"doctor", "--output", "json"}
}

// NewJobDriver returns a Driver that runs the self-check probe once and emits
// one finding event per subsystem entry.
func NewJobDriver(dataDir string, open OpenStore) executor.Driver {
	return jobDriver{dataDir: dataDir, open: open}
}

type jobDriver struct {
	dataDir string
	open    OpenStore
}

// Run implements executor.Driver. The exit code is the worst status: a FAIL
// finding fails the job, every other status is a state to read, not a fault —
// the same judgment `exocomp doctor` (text) makes when it reports before it
// fails on the data dir.
func (d jobDriver) Run(ctx context.Context, _ executor.Spec, sink executor.EventSink) (executor.Result, error) {
	view := Probe(ctx, d.dataDir, d.open)
	failing := false
	for _, e := range view {
		if e.Status == wire.HealthFail {
			failing = true
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return executor.Result{}, fmt.Errorf("health: doctor job: encode finding %q: %w", e.Name, err)
		}
		sink.Emit(executor.Event{
			Level:   executor.LevelSystem,
			Msg:     fmt.Sprintf("%s %s — %s", e.Name, e.Status, e.Detail),
			RawJSON: string(raw),
		})
	}
	if failing {
		return executor.Result{ExitCode: 1}, nil
	}
	return executor.Result{}, nil
}
