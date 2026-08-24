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
	// Post-v1 surfacing: the capabilities shipped after v1 that the glance
	// screen had not kept pace with.
	Sources   []sourceRow
	Schedules []scheduleRow
	Workflows []workflowRow // in-flight chains on the dashboard
	Self      selfObs
}

// sourceRow is one run-tree source (ADR 0010) on the sources surface: where a
// run's tree comes from, whether its ref is signed, and the commit its last
// sync landed on. Read-only — registration stays a CLI-and-local-auth path.
type sourceRow struct {
	Name    string
	Kind    string // git|hostpath|volpath
	URI     string
	Ref     string
	SyncSHA string
	Signed  bool
	Signers bool // an allowed-signers set is pinned (exocomp-owned trust)
	Enabled bool
}

// Short renders a sync sha at the length a human scans, never the full 40.
func (r sourceRow) Short() string {
	if len(r.SyncSHA) > 12 {
		return r.SyncSHA[:12]
	}
	return r.SyncSHA
}

// scheduleRow is one cron schedule: its kind, expression, next/last fire, and
// the outcome of the job its last fire enqueued — so a drift-check schedule's
// last drift result reads at a glance.
type scheduleRow struct {
	Name        string
	Kind        string // shell|backup|tofu-plan|play|… (free kind string)
	Cron        string
	Next        string
	LastRunbook string
	LastState   store.JobState
	LastJobID   int64
	HasLast     bool
	Enabled     bool
}

func (r scheduleRow) LastClass() string { return stateClass(r.LastState) }

// workflowRow is one step-ledger chain (ADR 0008): the provision->onboard->
// converge and TF->ansible handoff workflows. Steps carries the ordered ledger
// for the detail view; the dashboard shows the derived state and a step tally.
type workflowRow struct {
	ID    string
	State store.WorkflowState
	Total int
	Done  int
	Steps []workflowStepRow
}

func (r workflowRow) StateClass() string { return workflowStateClass(r.State) }

// Progress is the "3/5" tally the dashboard shows without opening the ledger.
func (r workflowRow) Progress() string {
	return itoa(int64(r.Done)) + "/" + itoa(int64(r.Total))
}

// workflowStepRow is one ordered step of a chain with its status and the job
// that ran (or is running) it.
type workflowStepRow struct {
	StepNo int
	Kind   string
	Status store.StepStatus
	JobID  *int64
}

func (r workflowStepRow) StatusClass() string { return stepStatusClass(r.Status) }

// policyRow is one CEL plan policy (the seal-time gate): which workspace it
// covers, whether a match denies the plan or routes it through the approval
// gate, and whether it is enabled. The expression is shown verbatim so an
// operator sees exactly what gates their applies.
type policyRow struct {
	Name      string
	Workspace string
	Action    string // deny|require_approval
	Expr      string
	Enabled   bool
}

// ActionClass colours the verdict: a deny is a hard stop (fail), a
// require_approval is a waiting-on-human gate (wait).
func (r policyRow) ActionClass() string {
	if r.Action == store.PolicyDeny {
		return "fail"
	}
	return "wait"
}

// selfObs is the dashboard's self-observability row: the control plane
// reporting on itself. Version is the running binary (the upgrade target); the
// litestream fields are the replication RPO age read the metrics registry
// exposes, surfaced here so an operator sees replication staleness without
// scraping.
type selfObs struct {
	Version      string
	LSConfigured bool
	LSAge        string // RPO age, or "unknown"
	LSClass      string // ok|wait|fail|dim
	LSDetail     string
}

// sourcesVM, schedulesVM, workflowsVM, workflowVM and policiesVM are the
// dedicated post-v1 surfaces. Each carries its own chrome plus its rows.
type sourcesVM struct {
	Page    page
	Sources []sourceRow
}

type schedulesVM struct {
	Page      page
	Schedules []scheduleRow
}

type workflowsVM struct {
	Page      page
	Workflows []workflowRow
}

type workflowVM struct {
	Page     page
	ID       string
	State    store.WorkflowState
	Steps    []workflowStepRow
	NotFound bool
}

func (vm workflowVM) StateClass() string { return workflowStateClass(vm.State) }

type policiesVM struct {
	Page     page
	Policies []policyRow
}

// runnersVM is the runners surface: a read-only inventory of the registered
// execution-environment recipes (byo-runner.md §2). A runner is registered for
// the deferred container tier and executes nothing today — registration,
// import, and removal stay CLI verbs; there is no mutation control here.
type runnersVM struct {
	Page    page
	Runners []runnerRow
}

// runnerRow is one registered runner's public face: its build mode, base
// image, package/tool pins, resource caps, and content hash. The bootstrap
// script body is deliberately absent — the row carries only what identifies
// and sizes it (byo-runner.md §7.1); `exocomp runner show` is the surface that
// prints a script body.
type runnerRow struct {
	Name        string
	Mode        string // cached|onthefly
	Base        string
	Packages    []string
	AnsibleCore string
	MemoryMax   string
	CPUQuota    string
	Hash        string
	Created     string
}

// OnTheFly reports whether the runner rebuilds every run (Model B) rather than
// building once and reusing the image until the recipe changes (Model A).
func (r runnerRow) OnTheFly() bool { return r.Mode == store.RunnerModeOnTheFly }

// Short renders the content hash at the length a human scans, mirroring
// sourceRow.Short for a sync sha.
func (r runnerRow) Short() string {
	if len(r.Hash) > 12 {
		return r.Hash[:12]
	}
	return r.Hash
}

// PackagesSummary renders the package pin list at a glance: the packages
// themselves when there are few, a count once the list would crowd the
// column.
func (r runnerRow) PackagesSummary() string {
	if len(r.Packages) == 0 {
		return "-"
	}
	if len(r.Packages) <= 3 {
		return strings.Join(r.Packages, ", ")
	}
	return fmt.Sprintf("%d packages", len(r.Packages))
}

// Caps renders the design §6 resource ceilings the deferred container tier
// would enforce — nothing executes them yet, so this is the recipe's stated
// intent, not a running limit.
func (r runnerRow) Caps() string {
	var parts []string
	if r.MemoryMax != "" {
		parts = append(parts, "mem "+r.MemoryMax)
	}
	if r.CPUQuota != "" {
		parts = append(parts, "cpu "+r.CPUQuota)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " · ")
}

// secretsVM is the secrets surface: a read-only inventory of the sealed values
// in the envelope. It carries only lifecycle metadata and the bindings that
// inject each secret — never the value, nonce, or ciphertext, which never leave
// the envelope.
type secretsVM struct {
	Page    page
	Secrets []secretRow
}

// secretRow is one sealed secret's public face: its name, kind, how it was
// minted, its lifecycle timestamps, an expiry staleness marker, and the
// run-secret bindings that inject it. The sealed material (value, nonce,
// ciphertext) is deliberately absent — invariant 4: secrets never render.
type secretRow struct {
	Name     string
	Kind     string
	Source   string // generated|user
	Created  string
	Rotated  string // clock, or "never"
	LastUsed string // clock, or "never"
	Expiry   expiryMarker
	Bindings []secretBindingRow
}

// secretBindingRow is one run-secret binding that injects a secret and the env
// var name it maps the value to at execution time. It carries no value.
type secretBindingRow struct {
	Binding string
	EnvName string
}

// expiryMarker is a secret's staleness state rendered as a labelled chip: an
// expired secret is a loud failure, an approaching expiry a wait, a distant or
// absent one dim. Never colour-only — the label always names the state.
type expiryMarker struct {
	Label string // EXPIRED | expires in Nd | no expiry
	Class string // fail | wait | dim
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

// stepStatusClass maps a workflow step's status to a chip class, matching the
// job palette: a running step breathes on the accent, a blocked step reads as a
// failure (a prior failure retired it), pending is dim.
func stepStatusClass(st store.StepStatus) string {
	switch st {
	case store.StepSucceeded:
		return "ok"
	case store.StepFailed, store.StepBlocked:
		return "fail"
	case store.StepRunning:
		return "run"
	default:
		return "dim"
	}
}

// workflowStateClass maps a whole chain's derived state to a chip class.
func workflowStateClass(st store.WorkflowState) string {
	switch st {
	case store.WorkflowSucceeded:
		return "ok"
	case store.WorkflowFailed:
		return "fail"
	default:
		return "run"
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

// orNever renders an optional lifecycle timestamp, using "never" for the nil
// case the rotate/last-used columns read.
func orNever(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return clock(*t)
}

// secretExpiry maps a secret's expiry to its staleness marker. A past expiry is
// a loud EXPIRED failure; an expiry inside a week is a wait; a further-off or
// absent one is dim. The label always names the state so the chip is never
// colour-only.
func secretExpiry(exp *time.Time, now time.Time) expiryMarker {
	if exp == nil {
		return expiryMarker{Label: "no expiry", Class: "dim"}
	}
	if !exp.After(now) {
		return expiryMarker{Label: "EXPIRED", Class: "fail"}
	}
	d := exp.Sub(now)
	days := int(d.Hours()) / 24
	label := "expires in " + itoa(int64(days)) + "d"
	if days < 1 {
		label = "expires in <1d"
	}
	class := "dim"
	if d < 7*24*time.Hour {
		class = "wait"
	}
	return expiryMarker{Label: label, Class: class}
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
