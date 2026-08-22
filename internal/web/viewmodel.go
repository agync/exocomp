package web

import (
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// page is the chrome every screen renders inside: which nav item is active, who
// is signed in, whether the listener is insecure, and the footer signal line.
type page struct {
	Title    string
	Nav      string
	Who      string
	Scope    string
	Insecure bool
	Signal   SignalLine
}

// SignalLine is the always-on footer: the dead-man switch, push transport, disk
// headroom, and version. It is the "is the control plane itself alive" strip.
type SignalLine struct {
	DeadMan string
	Ntfy    string
	Disk    string
	Version string
	Warn    bool
}

// dashboardVM is the glance screen: the first answer to "is everything OK?".
type dashboardVM struct {
	Page       page
	Banner     bannerVM
	NeedsYou   []approvalRow
	Queue      []jobRow
	Backups    []backupRow
	Workspaces []workspaceRow
}

// bannerVM carries the pause/silence/mute state that rides every screen until
// it lifts, with the count of alerts swallowed while it was up. The three holds
// are independent and align with the TUI's banner model: Paused is the reactive
// global kill switch (jobs held); Silenced is a scoped reactive silence; Muted
// is a notification mute (notify_mute) — jobs still run, only the push is held.
// Any combination can be true at once.
type bannerVM struct {
	Paused     bool
	Silenced   bool
	Muted      bool
	Swallowed  int
	Until      string
	MutedUntil string
}

func (b bannerVM) Active() bool { return b.Paused || b.Silenced || b.Muted }

// Keyword names every active hold, most severe first, so PAUSED (jobs held)
// always leads and MUTED (jobs still run) reads as the distinct, milder state.
func (b bannerVM) Keyword() string {
	var parts []string
	if b.Paused {
		parts = append(parts, "PAUSED")
	}
	if b.Silenced {
		parts = append(parts, "SILENCED")
	}
	if b.Muted {
		parts = append(parts, "MUTED")
	}
	return strings.Join(parts, " + ")
}

func (b bannerVM) Message() string {
	switch {
	case b.Paused:
		return "The reactive engine is paused by the global kill switch. Received alerts are swallowed; running jobs finish and schedules still fire."
	case b.Silenced:
		return "One or more scoped silences are active. Matching alerts are swallowed until they expire."
	default:
		msg := "Notifications are muted — jobs still run and outcomes are recorded, only the phone push is held."
		if b.MutedUntil != "" {
			msg = "Notifications are muted until " + b.MutedUntil + " — jobs still run and outcomes are recorded, only the phone push is held."
		}
		return msg
	}
}

// approvalRow is one pending decision on the dashboard's needs-you list and in
// the approvals surface.
type approvalRow struct {
	ID        int64
	JobID     int64
	Runbook   string
	Workspace string
	Deadline  string
	Expiring  bool
}

// jobRow is one job in the queue/jobs list.
type jobRow struct {
	ID       int64
	Runbook  string
	Kind     string
	Trigger  string
	State    store.JobState
	Age      string // oldest-queued age for queued rows; runtime otherwise
	ExitCode string
}

func (j jobRow) StateClass() string { return stateClass(j.State) }

// backupRow is one target's freshness on the dashboard.
type backupRow struct {
	Name      string
	Kind      string
	State     string
	Snapshot  string
	Age       string
	Freshness string // ok|wait|fail per 24h/48h thresholds
	Drill     bool
}

// workspaceRow is one tofu workspace with its drift marker.
type workspaceRow struct {
	Name    string
	Serial  int64
	Drift   bool
	LastJob string
}

// jobDetailVM is the streamed job view: its header plus the events seen so far.
type jobDetailVM struct {
	Page   page
	Job    jobRow
	Events []eventLine
}

type eventLine struct {
	Seq   int64
	Ts    string
	Level string
	Msg   string
}

// approvalVM is the phone approval surface: the seven-point contract rendered.
type approvalVM struct {
	Page      page
	ID        int64
	JobID     int64
	Runbook   string
	Kind      string
	Workspace string
	// Countdown is the fail-closed TTL text; Expired means the window closed.
	Countdown string
	Expired   bool
	Decided   bool
	Outcome   string
	Notice    string
	// Destroys are the resource lines that DELETE, rendered first, in red, with
	// no undo. Changes are the rest of the diff. Both come from the sealed plan.
	Destroys []planLine
	Changes  []planLine
	HasPlan  bool
	// Stale is set when the workspace serial moved after the plan: a refusal,
	// not a disabled button.
	Stale       bool
	StaleNotice string
	// Provenance is the standing guardrail copy and what matched.
	Provenance string
	CanDecide  bool
}

// planLine is one resource line of a plan diff. Secret-backed values never
// carry their value here: Redacted marks the ones the phone shows as
// [redacted · secret].
type planLine struct {
	Action   string // create|update|delete|replace
	Address  string
	Detail   string
	Redacted bool
}

// eventsVM is the events + evaluation trace screen.
type eventsVM struct {
	Page   page
	Events []eventRow
}

type eventRow struct {
	ID       int64
	Source   string
	Received string
	Name     string
	Outcome  store.EventOutcome
	Reason   string
	JobID    *int64
}

func (e eventRow) OutcomeClass() string { return outcomeClass(e.Outcome) }

// traceVM is one event's stored evaluation trace.
type traceVM struct {
	Page  page
	Event eventRow
	Steps []traceStep
}

type traceStep struct {
	Passed bool
	Step   string
	Detail string
	Reason string
}

// ---- helpers the templates call ----

func stateClass(s store.JobState) string {
	switch s {
	case store.StateSucceeded:
		return "ok"
	case store.StateFailed, store.StateTimedOut, store.StateInterrupted:
		return "fail"
	case store.StateAwaitingApproval:
		return "wait"
	case store.StateRunning:
		return "run"
	default:
		return "dim"
	}
}

func outcomeClass(o store.EventOutcome) string {
	switch o {
	case store.OutcomeFired:
		return "ok"
	case store.OutcomeError:
		return "fail"
	case store.OutcomeGated:
		return "wait"
	case store.OutcomeSuppressed:
		return "dim"
	default:
		return "dim"
	}
}

// humanAge renders a duration the way the surfaces read it: compact, past
// tense, aligned to the largest meaningful unit.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// backupFreshness maps an age to the 24h/48h ladder in the brief: quiet until
// 24h, a warning at 24h, loud at 48h.
func backupFreshness(age time.Duration) string {
	switch {
	case age >= 48*time.Hour:
		return "fail"
	case age >= 24*time.Hour:
		return "wait"
	default:
		return "ok"
	}
}
