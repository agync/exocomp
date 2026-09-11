package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
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
	// BackHref is the detail page's back-chevron target (SPEC §2.0): the
	// phone app bar swaps its menu button for a back chevron when set. Empty
	// renders the menu button. TitleSub stacks under Title ("Job #5" / the
	// mono playbook) on the phone shell; empty renders a single-line title.
	BackHref string
	TitleSub string
	// Actions is the phone app bar's action list (SPEC §2.0): the 36px
	// compact buttons (Raw log control, Rerun accent). It coexists with Add:
	// Add stays the desktop page-add verb, Actions the phone app-bar list.
	// Empty renders no slot.
	Actions []pageAction
	// Meta is the app bar's page-own fact slot (W7): "host · up 6d" on the
	// dashboard, a count on a list page. It is NOT the identity — that lives in
	// the powerline only. Empty renders no slot.
	Meta string
	// Flash is a one-line operator-facing result from a redirect-after-POST
	// (write.go). Never a secret value — those render once inline.
	Flash  string
	Signal SignalLine
	// PageClass is an extra class on <main> for page-scoped layout rules. The
	// job detail page uses it to make itself a no-page-scroll flex column so
	// the log box owns the scroll (T1); every other page leaves it empty.
	PageClass string
	// Add is the shell header's one primary verb (F9/F15): a labeled accent
	// button opening the page's add-modal. Nil renders no verb.
	Add *pageAdd
	// Badges carries the nav's actionable state counts (F7): fail on Checks,
	// wait on Sources/Approvals. A zero count renders no badge.
	Badges navBadges
}

// pageAdd is the header's primary verb: the modal id it opens and the button
// label. Text overrides the default "Add {label}" form (W10: a workspace's verb
// is "Configure", a runner's is "Add runner").
type pageAdd struct {
	Target string
	Label  string
	Text   string
}

// ButtonText is the header verb's visible label.
func (a *pageAdd) ButtonText() string {
	if a.Text != "" {
		return a.Text
	}
	return "Add " + a.Label
}

// pageAction is one app-bar action (SPEC §2.0): a 36px compact button, or a
// 34px glyph verb when Glyph is set (T6: header verbs are glyphs, right-
// aligned, at most two then a chevron menu). Kind names the accent: "control"
// is the quiet surface fill, "accent" the primary tint — the Raw log / Rerun
// pair on a job detail page.
type pageAction struct {
	Href  string
	Label string
	Kind  string // control|accent
	// Glyph names the stroke-SVG verb (row_actions.go) rendered instead of
	// the text label. aria-label stays the Label so the verb is announced.
	Glyph string
}

// navBadges is the per-nav actionable counts shown beside a nav item: how many
// probe targets are down (Checks), how many sources are never-synced, and how
// many approvals are pending. A count is the number of things needing a look,
// not a pill.
type navBadges struct {
	ChecksDown int
	Sources    int
	Approvals  int
}

// SignalLine is the always-on powerline: the dead-man switch, push transport,
// and disk headroom. It is the "is the control plane itself alive" strip; the
// running version is a /system concern, not a status-bar one. Each segment
// carries its signal state so the strip renders dot + word in that color (F10):
// a gap reads as a verb, never a grey fact.
type SignalLine struct {
	DeadMan      string
	DeadManState string
	Ntfy         string
	NtfyState    string
	Disk         string
	DiskState    string
}

// dashboardVM is the glance screen: the first answer to "is everything OK?".
// NeedsYou and Queue are the full-list fields shared with the dedicated
// /approvals and /jobs surfaces (handleApprovals, handleJobs) that reuse this
// struct; the dashboard handler itself never populates them. Approvals,
// Jobs, Workspaces, Sources, Checks and Schedules are the at-a-glance summary
// cards — counts plus the single attention detail each needs, never a full
// list.
type dashboardVM struct {
	Page     page
	Banner   bannerVM
	NeedsYou []approvalRow
	Queue    []jobRow

	Approvals  approvalSummary
	Jobs       jobSummary
	Workspaces workspaceSummary
	Sources    sourceSummary
	Checks     checksSummary
	Workflows  workflowSummary
	Schedules  scheduleSummary
	// Rules is the count-band tally: reactive rules are CLI-managed and
	// read-only on the web, so the card only ever counts them.
	Rules int
	// ApprovalFP is the fingerprint of the pending-approvals list as of
	// render (approvalsFP over the same ListApprovals rows the page shows),
	// handed to approvalsLive so the stream's ?fp= can prove the connect-time
	// state is already on screen and skip a reload-only snapshot (F4).
	ApprovalFP string
}

// approvalSummary is the dashboard's pending-approvals card: the open count
// and the soonest-to-expire item, the one line the card is allowed to show.
// The full list lives at /approvals.
type approvalSummary struct {
	Pending   int
	Runbook   string
	Workspace string
	Deadline  string
	Expiring  bool
}

// Subline is the pending-approvals card's one attention line: the
// soonest-to-expire approval's target and time left. Empty when nothing is
// pending.
func (c approvalSummary) Subline() string {
	if c.Pending == 0 {
		return ""
	}
	target := c.Runbook
	if c.Workspace != "" {
		target += " · " + c.Workspace
	}
	return target + " · " + c.Deadline
}

// jobSummary is the dashboard's queue card: jobs still moving right now,
// rendered as capped two-line rows in the /jobs language. Settled jobs
// (succeeded, failed, ...) never count or render here — that history belongs
// to the /jobs view, not the glance screen.
type jobSummary struct {
	Running int
	Queued  int
	// Rows are the active jobs as /jobs two-line rows, newest first and capped
	// at queueRowsCap: the glance shows the newest movers and counts the whole
	// set. Empty when Idle.
	Rows []jobRow
	// FailedRecently is jobs that settled failed inside the recent window
	// (jobFailedRecentWindow). Negative means not computed.
	FailedRecently int
	// LastJob is the newest settled job, rendered as the queue card's one
	// settled-history line: its runbook and a state chip. Nil when no job has
	// ever finished.
	LastJob *jobLastRow
	// Recent is the queue card's LAST N RUNS group (SPEC §3): the newest
	// settled jobs as jobRow rows, the settled history the card lists. The
	// Dashboard lane (Wave 2) populates and renders it and retires
	// FailedRecently/LastJob; both fields remain for the current queue card
	// until that lane lands (see lane report).
	Recent []jobRow
}

// queueRowsCap is how many active rows the queue card renders before the card
// stops being a glance screen; the headline counts the whole active set and
// the Open all jobs footer links the rest through to /jobs.
const queueRowsCap = 6

// Idle reports whether the active queue is empty.
func (j jobSummary) Idle() bool { return j.Running == 0 && j.Queued == 0 }

// Headline is the card's one big line: "N running · M queued".
func (j jobSummary) Headline() string {
	return itoa(int64(j.Running)) + " running · " + itoa(int64(j.Queued)) + " queued"
}

// workspaceSummary is the dashboard's workspace card: the managed total and
// how many carry an open drift approval.
type workspaceSummary struct {
	Total   int
	Drifted int
}

// sourceSummary is the dashboard's source card: the registered total and how
// many need attention — never synced, or disabled. NeverSynced + NeverNames
// drive the NEEDS ATTENTION band's chip + sentence for the never-synced case.
type sourceSummary struct {
	Total       int
	Attention   int
	NeverSynced int
	NeverNames  []string
}

// NeverSentence is the attention card's one line for never-synced sources:
// the names, or a generic line when more than two never synced.
func (s sourceSummary) NeverSentence() string {
	switch len(s.NeverNames) {
	case 1:
		return s.NeverNames[0] + " has never synced — nothing can run from it until it does."
	case 2:
		return s.NeverNames[0] + " and " + s.NeverNames[1] + " have never synced — nothing can run from them until they do."
	default:
		return itoa(int64(s.NeverSynced)) + " sources have never synced — nothing can run from them until they do."
	}
}

// checksSummary is the dashboard's probe-estate card: the registered total, the
// targets the sweep currently observes up, and how many checks are disabled.
// DOWN is not the attention signal — it is normal for an away/roaming host —
// a disabled check is (mirrors sourcesCard).
type checksSummary struct {
	Total        int
	TargetsUp    int
	TargetsDown  int
	TargetsTotal int
	Disabled     int
}

// scheduleSummary is the dashboard's schedule card: the registered total and
// how many are disabled. A disabled schedule has silently stopped firing —
// that is the attention signal the card carries (mirrors sourcesCard).
type scheduleSummary struct {
	Total    int
	Disabled int
}

// netgroupFormVM is the netgroup-add dialog's refusal state. ErrName and
// ErrCIDRs hold the translated sentences (forms/copy.txt) — the raw validator
// words go to the log at the same call site — and FixName, when set, is the
// one-click slug the fix link applies. ErrCIDRItem is the offending member,
// rendered mono ahead of ErrCIDRs when the field holds several values.
// FirstInvalid names the control to focus; Submitting drives the status line's
// "Adding" and disables the primary.
type netgroupFormVM struct {
	Name         string
	CIDRs        string
	ErrName      string
	ErrCIDRItem  string
	ErrCIDRs     string
	FixName      string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

// effective dereferences the form for rendering; nil is the at-rest form
// (help text, no refusals, dialog closed). A checksVM carries nil on a fresh
// GET and the filled form only on a 422 re-render.
func (f *netgroupFormVM) effective() netgroupFormVM {
	if f == nil {
		return netgroupFormVM{}
	}
	return *f
}

// Pending reports whether the form is mid-submit, driving the status line's
// "Adding" and the primary's disabled state. The primary is never disabled for
// validation — only while submitting.
func (f *netgroupFormVM) Pending() bool {
	return f != nil && f.Submitting
}

// ErrFormText is the whole-submit refusal for the status line; empty renders
// the line empty.
func (f *netgroupFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// nameField and cidrField build the reusable field's render state from the
// form's current values and refusals, so the netgroup panel stays a list of
// fieldInput calls and the per-field copy lives beside the form model.
func (f *netgroupFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "ng-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "home-cidr", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *netgroupFormVM) cidrField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "ng-cidr", Label: "CIDRs", Name: "cidrs", Value: v.CIDRs,
		Placeholder: "192.168.0.0/16, 10.0.0.0/8", Help: "Comma-separated. IPv4 or IPv6.",
		ErrItem: v.ErrCIDRItem, ErrMsg: v.ErrCIDRs,
		FirstInvalid: v.FirstInvalid == "cidrs",
	}
}

// workflowSummary is the dashboard's in-flight-workflows card.
type workflowSummary struct {
	InFlight int
}

// systemVM is the /system surface: the control plane's health of itself — the
// running binary version and Litestream replication RPO — plus backup
// freshness. Both moved off the glance dashboard (and the version off the
// powerline) so the dashboard stays the managed-estate view.
type systemVM struct {
	Page      page
	Self      selfObs
	Backups   []backupRow
	Operator  bool
	PublicURL string // daemon.public_url, "" when unset
}

// sourceRow is one run-tree source (ADR 0010) on the sources surface: where a
// run's tree comes from, whether its ref is signed, and the commit its last
// sync landed on. Read-only — registration stays a CLI-and-local-auth path.
type sourceRow struct {
	Name    string
	Kind    string // git|path
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

// NeverSynced reports whether a source is in the "waiting to sync" state: a git
// source with no recorded sync (no checkout yet), never a path source. A path
// source's tree is already on the box and read directly — there is no sync verb
// for it (`source sync` refuses a non-git source), so it is never a wait state
// with a verb (F5).
func (r sourceRow) NeverSynced() bool { return r.Kind != store.SourcePath && r.SyncSHA == "" }

// LastSyncLabel is the source detail's Last-sync fact: a path source has no sync
// to report, a never-synced git source names the verb that changes it, and a
// synced source shows its pinned commit.
func (r sourceRow) LastSyncLabel() string {
	switch {
	case r.Kind == store.SourcePath:
		return "n/a (path source)"
	case r.NeverSynced():
		return "never — sync to discover playbooks"
	default:
		return r.Short()
	}
}

// ShowEnabledColumn reports whether any source is disabled: the ENABLED column
// renders only when a row diverges from the default (F12/W17), so an all-enabled
// estate drops the empty column.
func (vm sourcesVM) ShowEnabledColumn() bool {
	for _, sc := range vm.Sources {
		if !sc.Enabled {
			return true
		}
	}
	return false
}

// scheduleRow is one cron schedule: its kind, expression, next/last fire, and
// the outcome of the job its last fire enqueued — so a drift-check schedule's
// last drift result reads at a glance. Argv/Target/ConcurrencyKey/JitterS/
// TolerateUnreachable and LastRun are the rest of the row's spec, shown only
// in its detail modal (schedulesList) — the table itself stays scannable.
type scheduleRow struct {
	Name                string
	Kind                string // shell|backup|tofu-plan|play|… (free kind string)
	Cron                string
	Next                string
	LastRunbook         string
	LastState           store.JobState
	LastJobID           int64
	HasLast             bool
	Enabled             bool
	Argv                []string
	Target              string
	ConcurrencyKey      string
	JitterS             int
	TolerateUnreachable bool
	LastRun             string
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
	// Operator gates the write controls (sources_write.go); SealAvailable is
	// false on a web-only surface holding no envelope, which the add-git form
	// needs to seal the generated deploy key.
	Operator      bool
	SealAvailable bool
}

type schedulesVM struct {
	Page      page
	Operator  bool
	Schedules []scheduleRow
	Prefill   schedulePrefill
}

// ShowEnabledColumn reports whether any schedule is disabled: the ENABLED
// column renders only when a row diverges from the default (F12/W17).
func (vm schedulesVM) ShowEnabledColumn() bool {
	for _, sc := range vm.Schedules {
		if !sc.Enabled {
			return true
		}
	}
	return false
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
	// Operator gates the add/rm controls (policies_write.go); readonly and
	// approve scopes keep the plain read-only listing.
	Operator bool
}

// runnersVM is the runners surface: an inventory of the registered
// execution-environment recipes (byo-runner.md §2). A runner is registered for
// the deferred container tier and executes nothing today — registration
// carries no secret material, so an operator can add or remove a definition
// here (runners_write.go); `exocomp runner import` and `show` stay CLI-only.
type runnersVM struct {
	Page     page
	Operator bool
	Runners  []runnerRow
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
	// Operator gates the write controls; SealAvailable is false on a web-only
	// process with no envelope, disabling add/rotate with a note.
	Operator      bool
	SealAvailable bool
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

// approvalRow is one pending decision on the /approvals surface.
type approvalRow struct {
	ID        int64
	JobID     int64
	Runbook   string
	Workspace string
	Deadline  string
	Expiring  bool
	// ExpiresAt is the raw deadline behind Deadline's rendered string — kept
	// so the dashboard card can pick the soonest-to-expire row without a
	// second query or a string-parse of Deadline.
	ExpiresAt time.Time
}

// jobRow is one job in the queue/jobs list.
type jobRow struct {
	ID      int64
	Runbook string
	Kind    string
	Trigger string
	// Source is the persisted job.Args.Source: the registered source this run
	// executed from, empty for a raw-path or ad-hoc run.
	Source   string
	State    store.JobState
	Age      string // oldest-queued age for queued rows; runtime otherwise
	ExitCode string
	// Argv is the job's own resolved command line — populated for jobDetail's
	// "Run again"/"Schedule" actions and their read-only argv preview; the
	// plain jobsList table never renders it.
	Argv []string
	// Title is what the PLAYBOOK cell and the run card title name: an ansible
	// run's playbook path relativized against its working directory, else the
	// runbook label.
	Title string
	// SourceMeta is the meta under the playbook: "{Source} · check mode".
	SourceMeta string
	// TriggerLabel/TriggerMeta render the TRIGGER cell's two lines — "Rerun" /
	// "of #4 · operator", "Manual" / "operator", "Schedule" / the schedule name.
	TriggerLabel string
	TriggerMeta  string
	// Reason is the D4 one-fact reason line ("2 hosts unreachable",
	// "exit 4 · 2 hosts unreachable", "exit 2 · syntax error"); empty on
	// SUCCEEDED and every non-terminal row. One terse fact, never a host list —
	// names live on the detail page.
	Reason string
	// Duration is the run's wall time ("41s"); FinishedRel the "16:31 today"
	// clock of when it ended; FinishedClock the HH:MM clock alone (the phone
	// RAN meta's "{HH:MM} · time" second line, SPEC §3).
	Duration      string
	FinishedRel   string
	FinishedClock string
	// CheckMode reports the run's --check flag for the meta line.
	CheckMode bool
}

func (j jobRow) StateClass() string { return stateClass(j.State) }

// needsOperator reports whether a job's terminal state still needs the operator
// (SPEC §3): failed and degraded runs carry a Rerun verb, every other state is
// quiet. The verb is the operator's cue — never a bare fact.
func needsOperator(s store.JobState) bool {
	return s == store.StateFailed || s == store.StateDegraded
}

// ReasonClass names the reason line's state colour: the degraded line reads in
// the wait signal, every other line in the default ink.
func (j jobRow) ReasonClass() string {
	if j.State == store.StateDegraded {
		return "reason reason--degraded"
	}
	return "reason"
}

// outputMeta renders the Output bar's meta line: "stdout + stderr · ended
// {hh:mm:ss}" on a terminal job, "stdout + stderr · {n} lines · streaming" on
// a live one (copy.txt). The count lives in the pin chip, never twice here.
func outputMeta(n int, terminal bool, ended string) string {
	if terminal {
		if ended != "" {
			return fmt.Sprintf("stdout + stderr · ended %s", ended)
		}
		return fmt.Sprintf("stdout + stderr · %d lines", n)
	}
	return fmt.Sprintf("stdout + stderr · %d lines · streaming", n)
}

// RunbookLabel strips the "rule:" prefix a rule-triggered job's runbook
// carries, so the job detail and the web jobs list read the rule name. It is
// display-only: the raw runbook stays the run's logic key (the approvals list
// renders it verbatim, and the CLI lists print it raw so a value read there is
// pasteable into --runbook). It is the one copy the web viewmodel and the CLI
// jobs get detail use (cmd calls web.RunbookLabel), so the display cannot
// drift between them.
func RunbookLabel(runbook string) string {
	runbook, _ = strings.CutPrefix(runbook, "rule:")
	return runbook
}

func (j jobRow) RunbookLabel() string { return RunbookLabel(j.Runbook) }

// Succeeded gates the Schedule button: a schedule replays a job's exact
// command, so offering it for anything short of a clean run would let an
// operator schedule a command already known to fail.
func (j jobRow) Succeeded() bool { return j.State == store.StateSucceeded }

// scheduleTitle is the Schedule button's tooltip: why it's disabled, or what
// it does when it isn't.
func scheduleTitle(j jobRow) string {
	if j.Succeeded() {
		return "Schedule this job to run on a cron"
	}
	return "Schedule available after a successful run"
}

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
	Page     page
	Job      jobRow
	Events   []eventLine
	Operator bool
	// Source is the registered source name this job ran from: the persisted
	// job.Args.Source when the row carries it, falling back to a Dir→source
	// reverse lookup for an old row that predates the field. Empty when the
	// job ran with no --source or the lookup found no match — "Run again"
	// links to /run (optionally scoped to that source) rather than
	// fabricating one.
	Source string
	// CanSchedule gates the "Schedule" action: true only for a terminal
	// ansible job with no source working directory, the one shape a "shell"
	// schedule can honestly replay (see jobs_schedule.go). Server-side
	// validation in handleJobSchedule is authoritative; this only hides the
	// button when it would just fail.
	CanSchedule bool
	// Rerunnable gates the "Run again" resubmit modal: true for any terminal job
	// that carries an argv to clone (a succeeded run included, an ad-hoc job
	// included — it does not depend on Source). Rerun holds the prefill.
	Rerunnable bool
	Rerun      jobRerunVM
	// LastSeq is the Seq of the last event this render included (0 when
	// none), passed to jobStream as ?since= so the SSE cursor picks up
	// exactly where the server-rendered tail left off instead of skipping
	// events written between render and connect.
	LastSeq int64
	// FiredRule is the rule this job's runbook named (runbook "rule:<name>"),
	// empty for a job no rule raised. TriggerEvent is the event that raised the
	// job, nil when none is on record — a manually enqueued job carrying a rule
	// runbook has no event row, so the rule line stands alone.
	FiredRule    string
	TriggerEvent *triggerEvent
	// RunCard is the run card: chips, the "Queued by ..." sentence, the
	// verdict number, and the RUN CONFIGURATION tiles.
	RunCard jobRunCardVM
	// Verdict is the verdict band, populated only on FAILED and DEGRADED.
	Verdict *jobVerdictVM
	// RerunFailedOn names the hosts this job's own recap left unreachable, for
	// the rerun dialog's "#N failed on {hosts}." suffix and its is-suggested
	// pre-highlight. Empty on a run that did not fail on unreachable hosts.
	RerunFailedOn []string
	// RerunOpen renders the rerun dialog open (?rerun=1, JS-off).
	RerunOpen bool
	// RefLabel is the source's "{Ref} @ {short sha}" per S1, empty when the
	// job ran from no source or the source row is gone. Ref is the branch
	// alone, the rerun dialog footer's "@ {ref}".
	RefLabel string
	Ref      string
	// PinState is the Output pin's server-set data-state (§2): "complete" on a
	// terminal page whose event list carries a terminal band, "cut" on a
	// terminal page without one (an interrupted/lost runner), "following" on a
	// live page. JS only moves the chip between following and paused (and the
	// done flash), so a terminal page never reads FOLLOWING TAIL without JS.
	PinState string
	// EndedClock is the terminal run's finished HH:MM:SS clock for the meta's
	// "ended {hh:mm:ss}"; empty on a live job or one with no finished_at.
	EndedClock string
}

// triggerEvent is the provenance link on a rule-triggered job's detail view:
// the event whose rule enqueued it, enough to link through to its trace.
type triggerEvent struct {
	ID     int64
	Source string
	Name   string
	Time   string
}

type eventLine struct {
	Seq   int64
	Ts    string
	Level string
	Msg   string
	// Band is the copy-formatted terminal-band line when this event is one of
	// the run's state bands (▸ running / ▸ succeeded / ▸ failed / ▸ degraded);
	// empty on a plain log line. BandClass is the band's state class
	// (run|ok|fail|wait).
	Band      string
	BandClass string
}

// hasTerminalBand reports whether an event list carries one of the run's
// terminal verdict bands (succeeded/failed/degraded), the distinction that
// makes the complete pin data-backed: a terminal page whose stream ends with
// no such line (an interrupted or lost runner) reads cut instead. The running
// band (class "run") is not terminal.
func hasTerminalBand(lines []eventLine) bool {
	for _, l := range lines {
		if l.Band != "" && l.BandClass != "run" {
			return true
		}
	}
	return false
}

// glyphCheck is the pin's 13px check glyph (SPEC §2 complete state); the dot
// covers every other state.
const glyphCheck = "M5 19L19 5M5 19l4 4"

// pinWord is the pin's keyword for a state; the count or scrolled-up tail
// rides the .n span beside it.
func pinWord(state string) string {
	switch state {
	case "cut":
		return "LOG CUT"
	case "complete":
		return "LOG COMPLETE"
	case "paused":
		return "PAUSED"
	default:
		return "FOLLOWING TAIL"
	}
}

// pinCount is the .n tail: " · {n} lines" on complete/cut, " · scrolled up"
// on paused, empty on following.
func pinCount(state string, n int) string {
	switch state {
	case "complete", "cut":
		return fmt.Sprintf(" · %d lines", n)
	case "paused":
		return " · scrolled up"
	}
	return ""
}

// jobRerunVM prefills the "Run again" resubmit modal from the original job. The
// toggles carry the original run's flags (Check/Diff detected in the stored
// argv, Privileged from Args.Privileged, Tolerate from Args.TolerateUnreachable);
// Limit/Tags are the editable values pulled from the argv's two-token flags.
// Command/Inventory/ExtraVars are the read-only identity — the run's own
// resolved command line and the names it carried, never a secret value.
type jobRerunVM struct {
	Check      bool
	Diff       bool
	Privileged bool
	Tolerate   bool
	Limit      string
	Tags       string
	Command    string
	Inventory  string
	ExtraVars  []string
}

// Summary is the rerun dialog footer's mono line naming the enabled options:
// "check · diff · tolerate unreachable". It is the server-rendered baseline;
// the toggles are real form fields, so a JS-off submit carries the same truth.
func (r jobRerunVM) Summary() string {
	var parts []string
	if r.Check {
		parts = append(parts, "check")
	}
	if r.Diff {
		parts = append(parts, "diff")
	}
	if r.Tolerate {
		parts = append(parts, "tolerate unreachable")
	}
	return strings.Join(parts, " · ")
}

// RerunQueueLabel is the rerun dialog's submit-button contract: the thing
// being queued and the ref it resolves against (F4). The `@ ref` is omitted
// when the source has no ref.
func (vm jobDetailVM) RerunQueueLabel() string {
	label := "Queue run — " + vm.RunCard.Title
	if vm.Ref != "" {
		label += " @ " + vm.Ref
	}
	return label
}

// configTile is one RUN CONFIGURATION tile: a label and a value, with the
// optional link the value resolves to and the state ring the cause tile wears
// (the Tolerate tile on a degraded run).
type configTile struct {
	Label string
	Value string
	Link  string
	// Cause marks the tile that carried the outcome's inset state ring.
	Cause bool
	// Wait tints the value with the wait signal ("on · settles degraded").
	Wait bool
}

// jobRunCardVM is the run card at the top of the job detail page: the chip
// row, the "Queued by ..." sentence, the verdict number, and the RUN
// CONFIGURATION tile grid.
type jobRunCardVM struct {
	State     store.JobState
	Tool      string
	CheckMode bool
	// RerunOf is the parent job's id when this run is a rerun; 0 otherwise.
	RerunOf  int64
	Title    string
	JobID    int64
	Sentence string
	// Verdict is "exit {code}" with " tolerated" appended on a degraded run;
	// empty on a job that never exited. Tolerated flags the smaller "tolerated"
	// tail.
	Verdict   string
	Tolerated bool
	// RanLine is "ran 00:00:41 · 16:26:38 → 16:27:19 · budget 600s"; empty on a
	// job that never finished.
	RanLine string
	// DegradedChip is the "2 hosts unreachable · hypnotoad, lrrr" wait-tint
	// chip, empty on every state but degraded.
	DegradedChip string
	Tiles        []configTile
}

// jobVerdictHost is one host row of the verdict band's table.
type jobVerdictHost struct {
	Host   string
	Class  string // fail | wait | ok
	Result string // UNREACHABLE | FAILED | OK
	Recap  string
}

// jobVerdictVM is the verdict band: the keyword line, the one sentence, the
// host table, and the invariant-3 verbs. It is populated only on FAILED and
// DEGRADED — healthy is quiet.
type jobVerdictVM struct {
	Keyword string
	Class   string // fail | wait — host-result vocabulary (.result.fail / .result.wait)
	// BandClass drives the band's own tint + inset ring: the CSS tokens are
	// verdict-band--failed / verdict-band--degraded (a different vocabulary from
	// the host rows' Class above).
	BandClass string // failed | degraded
	Sentence  string
	Hosts     []jobVerdictHost
	// Verb is the band's primary action link and its meta line; VerbSecond the
	// degraded band's second link.
	Verb           string
	VerbHref       string
	VerbMeta       string
	VerbSecond     string
	VerbSecondHref string
}

// recapHostRow is one PLAY RECAP host row plus the raw counts text ansible
// printed, for the verdict band's host table.
type recapHostRow struct {
	Host        string
	Changed     int
	Unreachable int
	Failed      int
	RawCounts   string
}

// jobOutcomeFacts is the parsed outcome the reason line and the verdict band
// read: the recap hosts, the unreachable/failed names, and whether an ansible
// syntax-error banner was the last error. It is computed once from a job's
// stored event stream (D1) and shared by the detail view and the list rows.
type jobOutcomeFacts struct {
	Hosts       []recapHostRow
	RecapFound  bool
	Unreachable []string
	Failed      []string
	SyntaxError bool
}

// parseJobOutcome reads a job's stored event stream for the facts a reason
// line and the verdict band need: the PLAY RECAP (already LevelInfo events,
// D1) and the last error banner. found stays false when no recap printed, so
// a run that errored before any host result fails closed rather than reading
// as "every host clean".
func parseJobOutcome(events []store.JobEvent) jobOutcomeFacts {
	var f jobOutcomeFacts
	var lines []string
	for _, e := range events {
		if e.Level == store.LevelInfo {
			lines = append(lines, e.Msg)
		}
	}
	hosts, found := executor.ParseRecapHosts(lines)
	f.RecapFound = found
	if !found {
		f.SyntaxError = syntaxErrorSeen(events)
		return f
	}
	for _, h := range hosts {
		f.Hosts = append(f.Hosts, recapHostRow{
			Host: h.Host, Changed: h.Changed, Unreachable: h.Unreachable,
			Failed: h.Failed, RawCounts: rawCountsFor(lines, h.Host),
		})
		if h.Unreachable > 0 {
			f.Unreachable = append(f.Unreachable, h.Host)
		}
		if h.Failed > 0 {
			f.Failed = append(f.Failed, h.Host)
		}
	}
	f.SyntaxError = syntaxErrorSeen(events)
	return f
}

// rawCountsFor returns the `ok=… changed=… skipped=…` text of one recap host
// row as ansible printed it, for the OK hosts' RECAP cell.
func rawCountsFor(lines []string, host string) string {
	inRecap := false
	for _, line := range lines {
		if strings.HasPrefix(line, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if !inRecap {
			continue
		}
		h, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(h) != host {
			continue
		}
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, "ok=") {
			return rest
		}
	}
	return ""
}

// syntaxErrorSeen reports whether the last LevelError event carries ansible's
// "ERROR!" banner — the stderr shape a syntax failure prints before any host
// ran (shell.go routes stderr to LevelError).
func syntaxErrorSeen(events []store.JobEvent) bool {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Level != store.LevelError {
			continue
		}
		return strings.HasPrefix(events[i].Msg, "ERROR!")
	}
	return false
}

// hostCountLine renders the reason line's host count, singular below two.
func hostCountLine(n int) string {
	if n == 1 {
		return "1 host unreachable"
	}
	return itoa(int64(n)) + " hosts unreachable"
}

// jobReason is the D4 one-terse-fact reason line: a degraded run names its
// unreachable count, a failed run its exit code plus the cause from the recap
// count or the syntax-error banner. Every other state carries no reason.
func jobReason(f jobOutcomeFacts, state store.JobState, exit string) string {
	switch state {
	case store.StateDegraded:
		if !f.RecapFound {
			return ""
		}
		return hostCountLine(len(f.Unreachable))
	case store.StateFailed:
		if exit == "" {
			return ""
		}
		cause := ""
		if f.RecapFound && len(f.Unreachable) > 0 {
			cause = " · " + hostCountLine(len(f.Unreachable))
		} else if f.SyntaxError {
			cause = " · syntax error"
		}
		return "exit " + exit + cause
	default:
		return ""
	}
}

// andList joins host names for a sentence: "hypnotoad and lrrr", or
// "a, b and c" past two.
func andList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// jobTitle names a job the way the PLAYBOOK cell and the run card do: an
// ansible run's playbook path relativized against its working directory (a
// source run reads "deploy/aio-sandbox.yml", not the checkout's absolute
// path), else the runbook label.
func jobTitle(j store.Job) string {
	if j.Kind == executor.KindAnsible && len(j.Args.Argv) > 0 && j.Runbook != "command" {
		return relTo(j.Args.Dir, j.Args.Argv[len(j.Args.Argv)-1])
	}
	return RunbookLabel(j.Runbook)
}

// relTo rebases a path onto dir when it names a path at or under it; a path
// outside dir, or a non-path token, passes through unchanged.
func relTo(dir, path string) string {
	if dir == "" || !strings.HasPrefix(path, dir) || len(path) <= len(dir) {
		return path
	}
	if dir[len(dir)-1] == '/' {
		return path[len(dir):]
	}
	if path[len(dir)] == '/' {
		return path[len(dir)+1:]
	}
	return path
}

// runDuration renders a run's wall time the way the RAN column reads it:
// "41s", "2m 14s", or "1h 5m".
func runDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// finishedRel renders a finished clock the way the RAN column reads it: today's
// runs as "16:31 today", older ones as "Sep 5 · 21:14".
func finishedRel(t, now time.Time) string {
	if t.UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
		return t.UTC().Format("15:04") + " today"
	}
	return t.UTC().Format("Jan 2 · 15:04")
}

// ranLine renders the verdict's "ran 00:00:41 · 16:26:38 → 16:27:19 · budget
// 600s" line; empty on a job that never started and finished.
func ranLine(j store.Job) string {
	if j.StartedAt == nil || j.FinishedAt == nil {
		return ""
	}
	d := j.FinishedAt.Sub(*j.StartedAt)
	line := fmt.Sprintf("ran %02d:%02d:%02d · %s → %s",
		int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60,
		j.StartedAt.UTC().Format("15:04:05"), j.FinishedAt.UTC().Format("15:04:05"))
	if t := time.Duration(j.Args.Timeout); t > 0 {
		line += fmt.Sprintf(" · budget %ds", int(t.Seconds()))
	}
	return line
}

// utcStamp renders a UTC timestamp as "2026-09-06 16:26:38 UTC".
func utcStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
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
	// DeadlineMS is the expiry as unix millis, the machine deadline the page's
	// ticking countdown reads. Countdown stays as the JS-off initial text.
	Countdown  string
	DeadlineMS int64
	Expired    bool
	Decided    bool
	Outcome    string
	Notice     string
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

// eventsVM is the events screen: the last-24h funnel card, the outcome and
// source filter chips, and the tray table of received alerts with the stored
// payload and evaluation trace inline. Filter is the active chip ("all" when
// none); Funnel is the 24h window while the chip counts and Total describe
// the rows in view and the whole store respectively. Empty hides the chips
// (R7: nothing to filter); NoMatch keeps them with the filtered-empty line.
type eventsVM struct {
	Page    page
	Funnel  store.EventOutcomeCounts
	Rows    []eventRow
	Chips   eventsChips
	Filter  string
	Total   int64
	Empty   bool
	NoMatch bool
}

// ChipCount is the count the chip named key carries — the rows in view that
// share its outcome or source split.
func (vm eventsVM) ChipCount(key string) int {
	switch key {
	case "all":
		return vm.Chips.All
	case "checks":
		return vm.Chips.Checks
	case "alerts":
		return vm.Chips.Alerts
	case "ignored":
		return vm.Chips.Ignored
	case "matched":
		return vm.Chips.Matched
	case "suppressed":
		return vm.Chips.Suppressed
	case "gated":
		return vm.Chips.Gated
	case "fired":
		return vm.Chips.Fired
	case "error":
		return vm.Chips.Error
	}
	return 0
}

// eventsChips are the per-chip counts. They count the rows in view (the page
// ListEvents returned), never the store: "all · 8" is the tray's rows, so the
// chips sum to the tray and not to a 90-day ledger.
type eventsChips struct {
	All                                 int
	Checks, Alerts                      int
	Ignored, Matched, Suppressed, Gated int
	Fired, Error                        int
}

// add tallies one rendered row into every chip it belongs to.
func (c *eventsChips) add(e eventRow) {
	c.All++
	switch e.Outcome {
	case store.OutcomeIgnored:
		c.Ignored++
	case store.OutcomeMatched:
		c.Matched++
	case store.OutcomeSuppressed:
		c.Suppressed++
	case store.OutcomeGated:
		c.Gated++
	case store.OutcomeFired:
		c.Fired++
	case store.OutcomeError:
		c.Error++
	}
	if e.Source == reactive.SourceCheck {
		c.Checks++
	} else {
		c.Alerts++
	}
}

type eventRow struct {
	ID       int64
	Source   string
	Received string
	// Rel is the row's "X ago" age (SPEC §2.5 line 2: "{HH:MM} · {rel} ago").
	Rel        string
	Name       string
	Rule       string
	Outcome    store.EventOutcome
	Reason     string
	ReasonLink string
	JobID      *int64
	Normalized string
	Trace      []traceStep
}

func (e eventRow) OutcomeClass() string { return outcomeClass(e.Outcome) }

// TokenGlyph, TokenWord, and TokenClass render the OUTCOME column's glyph and
// keyword per the closed enum (copy.txt): IGNORED — in ink, MATCHED ● in
// ink-200, SUPPRESSED and GATED ◐ in wait, FIRED ● in accent. error, which the
// lane keeps as a chip, reuses the ● in the fail colour. The glyph and the
// keyword always render together — never colour alone.
func (e eventRow) TokenGlyph() string {
	switch e.Outcome {
	case store.OutcomeMatched, store.OutcomeError, store.OutcomeFired:
		return "●"
	case store.OutcomeSuppressed, store.OutcomeGated:
		return "◐"
	default:
		return "—"
	}
}

func (e eventRow) TokenWord() string { return strings.ToUpper(string(e.Outcome)) }

func (e eventRow) TokenClass() string {
	switch e.Outcome {
	case store.OutcomeFired:
		return "fired"
	case store.OutcomeSuppressed, store.OutcomeGated:
		return "wait"
	case store.OutcomeMatched:
		return "matched"
	case store.OutcomeError:
		return "fail"
	default:
		return "ignored"
	}
}

// receivedClock renders the RECEIVED column: today's events by clock
// ("13:58:04"), older ones as "Wed 23:58".
func receivedClock(t, now time.Time) string {
	if t.UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
		return t.UTC().Format("15:04:05")
	}
	return t.UTC().Format("Mon 15:04")
}

// compactDuration renders a duration the way a reason column reads a cooldown
// or an approval TTL: "6h", "30m", "45s" — no trailing zero units.
func compactDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d == 0:
		return "0s"
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	case d >= time.Second && d%time.Second == 0:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	default:
		return d.String()
	}
}

// decodeSteps reads a stored trace_json into the row's step list, the same
// read the trace page makes. A malformed or absent trace yields nil so the
// pane renders "No trace recorded" rather than failing the page.
func decodeSteps(raw string) []traceStep {
	trace, err := reactive.DecodeTrace(raw)
	if err != nil {
		return nil
	}
	steps := make([]traceStep, 0, len(trace))
	for _, c := range trace {
		steps = append(steps, traceStep{Passed: c.Passed, Step: c.Step, Detail: c.Detail, Reason: c.Reason})
	}
	return steps
}

// prettyJSON re-indents a stored normalized payload for the expand pane. The
// row keeps the compact form; a malformed payload renders as stored.
func prettyJSON(raw string) string {
	if raw == "" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}

// enrichSuppressed maps a guardrail's stored reason to the design's reason
// copy: a silence reads "swallowed · silence {scope} · lifts {t}", a cooldown
// reads "cooldown {d} · next eligible {t}". A dedup or a daily cap keeps its
// stored sentence.
func enrichSuppressed(reason string) string {
	if strings.HasPrefix(reason, "silenced: ") {
		rest := reason[len("silenced: "):]
		until := strings.Index(rest, " until ")
		if until < 0 {
			return reason
		}
		scope, lifts := rest[:until], rest[until+len(" until "):]
		if j := strings.Index(lifts, " ("); j >= 0 {
			lifts = lifts[:j]
		}
		return "swallowed · silence " + silenceName(scope) + " · lifts " + lifts
	}
	if strings.HasPrefix(reason, "cooldown ") {
		return strings.Replace(reason, ", next eligible ", " · next eligible ", 1)
	}
	return reason
}

// silenceName drops the scope kind the engine prefixes ("host pve-02" reads
// as the design's "silence pve-02", "alert dns" as "dns").
func silenceName(scope string) string {
	for _, prefix := range []string{"alert ", "host "} {
		if strings.HasPrefix(scope, prefix) {
			return scope[len(prefix):]
		}
	}
	return scope
}

// traceVM is one event's stored evaluation trace, plus — for a check
// transition — the hysteresis ledger that produced it (the "WHY THIS COUNTED"
// card). Both come from the event row, never recomputed.
type traceVM struct {
	Page  page
	Event eventRow
	Steps []traceStep
	// CheckObservations is a check event's transition ledger, newest last;
	// nil for alert and non-check events.
	CheckObservations []checkObservationRow
	// StartedAt is the daemon's start time, for the cold-start body of the
	// ledger card.
	StartedAt string
}

type traceStep struct {
	Passed bool
	Step   string
	Detail string
	Reason string
}

// checkObservationRow is one probe of a check transition's hysteresis ledger.
type checkObservationRow struct {
	ObservedAt  string
	Status      string
	Consecutive int
}

// checkHysteresis is the observation window the sweep must hold before a
// transition fires. It mirrors reactive.DefaultHysteresis; the web renders the
// ledger lines with it (a divergence there would mislabel the card).
const checkHysteresis = 3

// LedgerLines renders the hysteresis ledger as the "observed {status}
// {hh:mm:ss} · {i} of 3" lines, with the transition line carrying the ok
// marker. The last observation is the one that crossed the window.
func (vm traceVM) LedgerLines() string {
	lines := make([]string, 0, len(vm.CheckObservations))
	for i, o := range vm.CheckObservations {
		var t string
		if ts, err := time.Parse(time.RFC3339, o.ObservedAt); err == nil {
			t = ts.UTC().Format("15:04:05")
		} else {
			t = o.ObservedAt
		}
		marker := " "
		if i == len(vm.CheckObservations)-1 {
			marker = "→"
		}
		lines = append(lines, fmt.Sprintf("observed %-4s %s   %d of %d %s",
			o.Status, t, o.Consecutive, checkHysteresis, marker))
	}
	return strings.Join(lines, "\n")
}

// ---- helpers the templates call ----

func stateClass(s store.JobState) string {
	switch s {
	case store.StateSucceeded:
		return "ok"
	case store.StateFailed, store.StateTimedOut, store.StateInterrupted:
		return "fail"
	case store.StateDegraded:
		// A tolerated degradation is a wait state: not a failure, never paged,
		// but the skipped hosts were not configured and still need the operator.
		return "wait"
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

// uptimeLabel renders the app bar's "up 6d 04h" fact from the daemon's start
// time (W7). It reads finer than humanAge when the process has been up a while.
func (s *Server) uptimeLabel() string {
	up := time.Since(s.startedAt)
	if up < 0 {
		up = 0
	}
	h := int(up.Hours())
	if h >= 24 {
		return fmt.Sprintf("up %dd %02dh", h/24, h%24)
	}
	return "up " + humanAge(up)
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
