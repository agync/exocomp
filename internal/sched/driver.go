package sched

import (
	"context"
	"fmt"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// Schedule kinds, aliased from internal/store so callers need one import for
// both the stored row and the queue Spec.Kind a fire produces. KindTofuPlan
// and KindPlay are the odd ones out: their fires are ordinary tofu and ansible
// jobs, so the daemon's own drivers serve them and this package's router never
// sees one.
const (
	KindShell    = store.ScheduleShell
	KindBackup   = store.ScheduleBackup
	KindTofuPlan = store.ScheduleTofuPlan
)

// NewDriver returns a Driver that dispatches on spec.Kind: KindShell to
// shell, KindBackup and backup.DrillJobKind to backup. A Queue takes exactly
// one Driver, but a scheduler fires argv jobs, backup jobs, and restore-drill
// jobs into the same queue, so something has to route between the underlying
// drivers. A drill routes to the same backup driver a real backup does — it
// is a *backup.Runner underneath, and Runner.Run itself tells the two kinds
// apart — so this router never grows a third slot for it.
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
	case KindBackup, backup.DrillJobKind:
		return d.backup.Run(ctx, spec, sink)
	default:
		return executor.Result{}, fmt.Errorf("sched: no driver for kind %q", spec.Kind)
	}
}
