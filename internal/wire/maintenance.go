package wire

import "time"

// KindMaintenance is the kind of a drain-readiness answer.
const KindMaintenance = "maintenance.readiness"

// KindMaintenanceReload is the kind of a schedule-reload answer.
const KindMaintenanceReload = "maintenance.reload"

// KindMaintenanceRestart is the kind of a restart request or its status answer.
const KindMaintenanceRestart = "maintenance.restart"

// MaintenanceView is the "safe to reboot?" answer a draining operator polls: is
// the daemon cordoned, how many of its jobs are still in flight, and — the one
// field a script branches on — whether both conditions for a clean reboot hold.
// It carries counts and states only, never a job's values (invariant 4).
type MaintenanceView struct {
	Cordoned     bool `json:"cordoned"`
	InFlight     int  `json:"in_flight"`
	SafeToReboot bool `json:"safe_to_reboot"`
}

// NewMaintenanceView derives the readiness answer. Safe is the conjunction the
// CLI would otherwise have to recompute: a daemon is safe to reboot only once it
// is cordoned (taking no new work) AND nothing it owns is still running.
func NewMaintenanceView(cordoned bool, inFlight int) MaintenanceView {
	return MaintenanceView{
		Cordoned:     cordoned,
		InFlight:     inFlight,
		SafeToReboot: cordoned && inFlight == 0,
	}
}

// ReloadView is what a schedule reload reports: how many enabled schedules the
// daemon now holds. It carries counts only (invariant 4).
type ReloadView struct {
	Schedules int `json:"schedules"`
}

// NewReloadView builds the reload answer.
func NewReloadView(schedules int) ReloadView {
	return ReloadView{Schedules: schedules}
}

// RestartView is a restart request's answer and the status a `restart --wait`
// polls: whether a drain-then-re-exec is pending, how many jobs are still in
// flight, and the deadline past which the daemon aborts (or, with --force,
// cancels the rest and restarts). It carries counts and a time only, never a
// job's values (invariant 4).
type RestartView struct {
	Pending  bool   `json:"pending"`
	InFlight int    `json:"in_flight"`
	Deadline string `json:"deadline,omitempty"`
}

// NewRestartView builds the restart answer. A zero deadline (no restart pending)
// is omitted rather than sent as the zero time, so a script reads "no pending
// deadline" instead of year 1.
func NewRestartView(pending bool, inFlight int, deadline time.Time) RestartView {
	v := RestartView{Pending: pending, InFlight: inFlight}
	if pending && !deadline.IsZero() {
		v.Deadline = deadline.UTC().Format(time.RFC3339)
	}
	return v
}
