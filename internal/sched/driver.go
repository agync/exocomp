package sched

import (
	"context"
	"fmt"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// Schedule kinds, aliased from internal/store so callers need one import for
// both the stored row and the queue Spec.Kind a fire produces.
const (
	KindShell  = store.ScheduleShell
	KindBackup = store.ScheduleBackup
)

// NewDriver returns a Driver that dispatches on spec.Kind: KindShell to
// shell, KindBackup to backup. A Queue takes exactly one Driver, but a
// scheduler fires both argv jobs and backup jobs into the same queue, so
// something has to route between the two underlying drivers.
func NewDriver(shell, backup executor.Driver) executor.Driver {
	return kindDriver{shell: shell, backup: backup}
}

type kindDriver struct {
	shell, backup executor.Driver
}

// Run implements executor.Driver.
func (d kindDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	switch spec.Kind {
	case KindShell:
		return d.shell.Run(ctx, spec, sink)
	case KindBackup:
		return d.backup.Run(ctx, spec, sink)
	default:
		return executor.Result{}, fmt.Errorf("sched: no driver for kind %q", spec.Kind)
	}
}
