package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// page is the chrome every screen renders inside: which nav item is active, who
// is signed in, whether the listener is insecure, and the footer signal line.
type page struct {
	Title    string
	Nav      string
	Who      string
	Scope    string
	Insecure bool
	// Base is the origin sub-path the surface is served under ("" at the root
	// origin). pageFor fills it from the server's --base-path; the URL sweep
	// re-prefixes root-absolute hrefs from it.
	Base string
	// BackHref is the detail page's back-chevron target (SPEC §2.0): the
	// phone app bar swaps its menu button for a back chevron when set. Empty
	// renders the menu button. BackLabel upgrades that back affordance to a
	// worded `‹ {label}` quiet link (R8's `‹ targets` on the backup-history
	// page); empty keeps the glyph chevron. TitleSub stacks under Title
	// ("Job #5" / the mono playbook) on the phone shell; empty renders a
	// single-line title.
	BackHref  string
	BackLabel string
	TitleSub  string
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

// checkFormVM is the check-add dialog's refusal state (forms/SPEC.md). Err*
// hold the translated sentences — the raw validator words go to the log at
// the same call site — and FixName is the one-click slug the fix link applies.
// Targets carries the repeated target inputs so a refusal re-renders every
// typed row. FirstInvalid names the control to focus; Submitting drives the
// status line's verb and disables the primary.
type checkFormVM struct {
	Name          string
	Kind          string
	Interval      string
	Targets       []string
	Expect        string
	ErrName       string
	FixName       string
	ErrKind       string
	ErrInterval   string
	ErrTargetItem string
	ErrTarget     string
	ErrExpect     string
	ErrForm       string
	FirstInvalid  string
	Submitting    bool
}

func (f *checkFormVM) effective() checkFormVM {
	if f == nil {
		return checkFormVM{}
	}
	return *f
}

func (f *checkFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *checkFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

func (f *checkFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "ck-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "at-home", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *checkFormVM) intervalField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "ck-interval", Label: "Interval (a duration)", Name: "interval",
		Value: v.Interval, Placeholder: "5m",
		Help:   "A Go duration — 5m, 90s, 1h.",
		ErrMsg: v.ErrInterval, FirstInvalid: v.FirstInvalid == "interval",
	}
}

func (f *checkFormVM) expectField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "ck-expect", Label: "HTTP expected status (optional, http kind only)",
		Name: "expect", Value: v.Expect, Placeholder: "200",
		Help:   "A whole status code, like 200 or 404.",
		ErrMsg: v.ErrExpect, FirstInvalid: v.FirstInvalid == "expect",
	}
}

// kindSelected reports whether the add form's kind select should show k: the
// typed value on a 422 re-render, the tcp default at rest.
func (f *checkFormVM) kindSelected(k string) bool {
	v := f.effective()
	if v.Kind == "" {
		return k == "tcp"
	}
	return v.Kind == k
}

// targetInputs is the repeated target field's rows: the typed values on a 422
// re-render, else the four baseline rows, capped at the same ten the Add
// target control enforces so the JS-off path cannot grow the DOM unbounded.
func (f *checkFormVM) targetInputs() []string {
	v := f.effective()
	out := v.Targets
	for i := len(out); i < 4; i++ {
		out = append(out, "")
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// targetPlaceholder names the shape a target row is an example of; the first
// three rows carry a worked example, later rows are blank.
func targetPlaceholder(i int) string {
	switch i {
	case 0:
		return "laptop=192.168.1.50:22"
	case 1:
		return "nas=192.168.1.60:22"
	case 2:
		return "web=https://web.lan/health"
	default:
		return ""
	}
}

// workflowSummary is the dashboard's in-flight-workflows card.
type workflowSummary struct {
	InFlight int
}

// systemVM is the /system self-care surface (SPEC §0): the running binary's
// build stamp and host (the identity strip), the four leg cards in the 2×2
// grid, and the powerline of per-leg anchors. Version/Commit/Platform/Host/
// Uptime feed the identity strip; BackupsWord/BackupsState feed the powerline's
// backups segment (the same rows the card draws, never a second read).
type systemVM struct {
	Page         page
	Self         selfObs
	Backups      []backupRow
	Operator     bool
	PublicURL    string // daemon.public_url, "" when unset
	BasePath     string // the --base-path the surface is served under, "" at the root
	Version      string
	Commit       string
	Platform     string // "{os}/{arch}" (runtime.GOOS/runtime.GOARCH)
	Host         string // the host the daemon runs on, "" when the OS cannot answer
	Uptime       string // "up 6d 04h" from the daemon start time
	BackupsWord  string // "backups ok" | "backups none" | "backups {n} failing"
	BackupsState string // ok|wait|fail — the card chip's colour for the strip
	// BackupsAge is the Backups card's FIGURE: the ONE age rule (the newest
	// SUCCEEDED run across enabled targets, LastSucceededBackupTime) — the
	// same read the Dead-man card quotes, so the two cards agree. "—" (dim)
	// when nothing has ever succeeded; BackupsAgeClass carries the freshness
	// ladder's colour for the figure.
	BackupsAge      string
	BackupsAgeClass string // ok|wait|fail|dim
	// Deadman is the dead-man leg's card state (SPEC §1). The age it quotes is
	// the store's ONE rule (LastSucceededBackupTime), the same read the
	// powerline's vm.Page.Signal.DeadMan makes — never a second computation.
	Deadman deadmanVM
	// Ntfy is the ntfy leg's judgment (SPEC §2): the ON/REFUSED/OFF state, the
	// topic label, and the sealed keys' presence the card renders. Presence,
	// never values.
	Ntfy ntfyCardVM
	// Litestream is the leg's own VM: the six word-states and the facts the
	// card and the /system/litestream page render, plus the generated config
	// for the ▾ view. It is one derivation (deriveLitestream) with two faces.
	Litestream litestreamVM
	// Doctor is the identity strip's Doctor glyph meta (SPEC §7 A1): the
	// newest self-check job's age and finding count, linking to the job page.
	Doctor doctorMeta
	// Kit is the identity strip's Recovery-kit meta (SPEC §7 A3): the date the
	// kit was last printed and how many invalidating changes landed since.
	Kit kitMeta
	// KitText is the regenerated kit the modal shows and the Download verb
	// serves — the envelope's public render (recipients, salt, recipe). It is
	// "" when the surface holds no envelope to render from.
	KitText string
	// PubForm is the identity strip's public-URL Change form refusal state
	// (forms/SPEC.md, SPEC §7 A7): nil at rest, the filled form on a 422
	// re-render. Value echoes the typed value so a refusal keeps it; ErrMsg is
	// the field meta's ▲ sentence, ErrForm the form-status line's.
	PubForm *pubFormVM
}

// pubFormVM is the public-URL Change form's refusal state. Value echoes the
// typed value on a 422 re-render; ErrMsg carries the validPublicURL guard's
// human copy on the field meta (never a flash), ErrForm a whole-submit failure
// on the 20px status line. Submitting drives the pending state.
type pubFormVM struct {
	Value      string
	ErrMsg     string
	ErrForm    string
	Submitting bool
}

func (f *pubFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *pubFormVM) ErrMsgText() string {
	if f == nil {
		return ""
	}
	return f.ErrMsg
}

func (f *pubFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// ValueFor resolves the control's value: the typed value on a 422 re-render,
// else the stored public URL.
func (f *pubFormVM) ValueFor(current string) string {
	if f != nil {
		return f.Value
	}
	return current
}

// doctorMeta is the /system identity strip's Doctor meta (SPEC §7 A1). HasRun
// is false until a self-check has been queued — the strip then renders the
// glyph alone, never a fabricated age. Count is the number of findings that
// need attention (warn|fail) in the newest self-check; Stale tints the meta
// when there are any.
type doctorMeta struct {
	HasRun bool
	JobID  int64
	Age    string
	Count  int
	Stale  bool
}

// kitMeta is the /system identity strip's Recovery-kit meta (SPEC §7 A3): the
// date the kit was last printed ("" when it has never been printed) and how
// many invalidating changes landed since — the honest staleness accounting
// behind "N changes since". Stale tints the meta when a change landed since
// the print.
type kitMeta struct {
	Printed string
	Changes int
	Stale   bool
}

// litestreamObserved is everything the daemon observes about the Litestream
// leg (SPEC §3): the persisted replica target, the sidecar binary's presence,
// and the replica's newest frame. deriveLitestream is a pure function of these
// observations, so the six-word state machine is testable without the binary
// installed and the card, the leg page, and the powerline never disagree.
type litestreamObserved struct {
	raw      string // SettingReplicaTarget verbatim, "" when unset
	remote   bool   // the parsed target's Remote() verdict (target.go)
	parseErr error  // non-nil when the stored target no longer parses
	binPath  string // litestream.Presence() path, "" when the binary is absent
	version  string // the version line ("" when absent or the probe failed)
	age      time.Duration
	mtime    time.Time
	frameNo  int64 // the newest frame's number, 0 when unreadable
	freshOK  bool  // litestream.ReplicaFreshness ok
	freshErr error
	ack      bool // SettingReplicaAck == "true"
	host     string
}

// litestreamVM is the Litestream leg of the /system self-care page (SPEC §3):
// the derived word-state plus the facts the card and the /system/litestream
// page render, and the generated config for the ▾ view. Read-only by design —
// the daemon generates config and observes, it never runs litestream (ADR
// 0012) — so the VM carries no verb that mutates anything.
type litestreamVM struct {
	State      string // REPLICATING|STALE|BEHIND|REMOTE|NO REPLICA|NO SIDECAR|UNPARSEABLE
	StateClass string // ok|wait|fail|dim — the chip, ring and figure colour
	Figure     string
	FigureNote string
	Replica    string // scrubbed target URI, "" when none
	Checkpoint string // "frame {n} · {HH:MM:SS}", "" when unobservable
	Sidecar    string // the sidecar fact, "" on NO SIDECAR
	Encryption string // the remote ack fact, "" unless REMOTE
	ConfigCmd  string // "exocomp litestream config" (the NO SIDECAR config fact)
	SetOne     string // "exocomp litestream target ‹uri›" (NO REPLICA / UNPARSEABLE)
	Fix        string // "install litestream on {host}, then doctor" (NO SIDECAR)
	Sentence   string
	Config     string // the generated yml, "" when ConfigErr is set
	ConfigErr  string // the config view's refusal, "" at rest
}

// litestreamPageVM is the /system/litestream surface (SPEC §3.2 + §7 A6): the
// full-width card plus the ▾ config view, both driven by the same litestreamVM
// the /system card renders, and the Set-target form (A6) that persists the
// replica target and the unencrypted-remote ack.
type litestreamPageVM struct {
	Page page
	LS   litestreamVM
	// Form is the Set-target form's render state (SPEC §7 A6): the target and
	// ack values the controls show at rest, plus the refusal state the write
	// handler fills on a 422. The daemon cannot verify a replica target (no
	// probe exists), so this is ALL the form writes — the two settings — and
	// the CLI echo is the honest boundary.
	Form litestreamFormVM
}

// litestreamFormVM is the Set-target form's render state (SPEC §7 A6). Target
// echoes the typed URI on a 422 so the operator can correct it; Ack is the
// checkbox's state. Whether the daemon can reach the target is deliberately
// not a field: the daemon cannot verify a replica, so the form writes the two
// settings and no more.
type litestreamFormVM struct {
	Target       string
	Ack          bool
	ErrTarget    string
	ErrAck       string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *litestreamFormVM) effective() litestreamFormVM {
	if f == nil {
		return litestreamFormVM{}
	}
	return *f
}

func (f *litestreamFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *litestreamFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// ErrTargetText and ErrAckText dereference the field refusals nil-safe (the
// page renders with a nil form at rest), so the template never touches the
// pointer fields directly.
func (f *litestreamFormVM) ErrTargetText() string {
	if f == nil {
		return ""
	}
	return f.ErrTarget
}

func (f *litestreamFormVM) ErrAckText() string {
	if f == nil {
		return ""
	}
	return f.ErrAck
}

// targetField is the replica-uri control on the landed fieldInput anatomy: the
// typed value on a 422, the stored target at rest. The value renders as stored
// (the operator's own config — an edit control, not a fact), so a target that
// embeds credentials carries the same cleartext warning the CLI gives at the
// point of storage.
func (f *litestreamFormVM) targetField() fieldMeta {
	v := f.effective()
	help := "litestream.replica_target · a path, file://, s3:// or sftp:// — the daemon cannot verify it."
	if backup.HasURIUserinfo(v.Target) {
		help = "litestream.replica_target · this URI embeds credentials — stored in cleartext, as the CLI warns."
	}
	return fieldMeta{
		ID: "ls-target", Label: "Replica-uri", Name: "replica_target",
		Value: v.Target, Placeholder: "/mnt/nas/exocomp-replica · s3://bucket/prefix · sftp://host/db",
		Help: help, ErrMsg: v.ErrTarget,
		FirstInvalid: v.FirstInvalid == "replica_target",
	}
}

// ackChecked is the checkbox's checked state: the stored ack at rest, the
// submitted value on a 422 re-render.
func (f *litestreamFormVM) ackChecked() bool {
	if f == nil {
		return false
	}
	return f.Ack
}

// cliEcho is the CLI command the form's values correspond to (SPEC §7 A6): the
// target with --ack-unencrypted when the typed target is remote and acked. The
// echo scrubs any userinfo the target carries — a rendered page never echoes a
// credential (invariant 4).
func (f *litestreamFormVM) cliEcho() string {
	v := f.effective()
	scrubbed := scrubReplica(v.Target)
	if scrubbed == "" {
		return "same as exocomp litestream target ‹uri›"
	}
	if target, err := litestream.ParseTarget(v.Target); err == nil && target.Remote() && v.Ack {
		return "same as exocomp litestream target " + scrubbed + " --ack-unencrypted"
	}
	return "same as exocomp litestream target " + scrubbed
}

// deadmanVM is the dead-man leg's render state (SPEC §1): the state word and
// class, the figure that changes MEANING per state (time-left → time-since)
// but never its slot, and the facts every state draws. The age is the store's
// ONE rule (LastSucceededBackupTime) — the handler quotes it once and nothing
// here recomputes it.
type deadmanVM struct {
	State          string // "none" | "armed" | "tripped"
	StateClass     string // wait | ok | fail
	StateWord      string // NONE | ARMED | TRIPPED
	Figure         string // "31h 12m" | "—" | "49h 02m"
	FigureClass    string // dim | "" | fail — the figure's ink per state
	FigureMeta     string
	Threshold      string // the raw threshold ("48h"), "" when unset
	LastGood       string // "{age} ago · {target} #{snapshot}" or "never"
	Heartbeat      string
	HeartbeatClass string // "" | fail — TRIPPED's heartbeat fact reads loud
	NewestRuns     string // TRIPPED's per-target newest-run line, "" otherwise
	Paged          string // "ntfy · once · {age} ago", "" unless TRIPPED — the fact's presence
	PagedChannel   string // "ntfy" — the fact's loud word
	PagedMeta      string // " · once · {age} ago" — the dim rest of the fact
	QueueTarget    string // TRIPPED's Queue-backup target, "" when none enabled
	Sentence       string
	Operator       bool
	// Form is the Arm dialog's refusal state (forms/SPEC.md): nil at rest, the
	// filled form with field refusals on a 422 re-render.
	Form *deadmanFormVM
}

// deadmanFormVM is the Arm dialog's refusal state. Threshold echoes the typed
// segment on a 422; the heartbeat value is NEVER carried back — the field
// renders empty always (invariant 4: sealed is a presence, never a value).
type deadmanFormVM struct {
	Threshold    string
	ErrThreshold string
	ErrHeartbeat string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
	// Open re-opens the dialog on the JS-off 422 re-render.
	Open bool
}

// deadmanDefaultThreshold is the Arm dialog's segmented default: 48h, the
// backup freshness ladder's fail step, so the Backups chip and the dead-man
// agree (SPEC §1.2).
const deadmanDefaultThreshold = "48h"

func (f *deadmanFormVM) effective() deadmanFormVM {
	if f == nil {
		return deadmanFormVM{}
	}
	return *f
}

func (f *deadmanFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *deadmanFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// ErrThresholdText and ErrHeartbeatText dereference the field refusals nil-safe
// (the dialog renders with a nil form at rest), so the template never touches
// the pointer fields directly.
func (f *deadmanFormVM) ErrThresholdText() string {
	if f == nil {
		return ""
	}
	return f.ErrThreshold
}

func (f *deadmanFormVM) ErrHeartbeatText() string {
	if f == nil {
		return ""
	}
	return f.ErrHeartbeat
}

// thresholdSelected reports whether the segmented switch's k-th position is
// on: the typed value on a 422 re-render, else the card's current threshold,
// else the 48h default — exactly one segment is on.
func (f *deadmanFormVM) thresholdSelected(current, k string) bool {
	v := current
	if f != nil && f.Threshold != "" {
		v = f.Threshold
	}
	if v == "" {
		v = deadmanDefaultThreshold
	}
	return v == k
}

// viewDeadmanVM is the /system/deadman leg page (SPEC §0.2 phone rows land on
// it): the page chrome around the same full-width card the /system grid
// renders. The card carries its own verbs and the Arm dialog.
type viewDeadmanVM struct {
	Page    page
	Deadman deadmanVM
}

// ntfyCardVM is the ntfy leg's render state (SPEC §2): the channel judgment
// (ON/REFUSED/OFF), the topic label (notify.ntfy_topic — the one unsealed
// display string), the server class (a fact about the URL, never the URL), the
// Detail setting, and the sealed keys' presence. Values never render — a
// sealed key is SEALED or MISSING, nothing else.
type ntfyCardVM struct {
	State       NtfyState
	TopicLabel  string
	ServerClass string
	Detail      string
	URLSealed   bool
	TopicSealed bool
	TokenSealed bool
}

// ChipClass colours the ntfy leg's state chip: ok for ON, fail for REFUSED,
// dim for OFF.
func (n ntfyCardVM) ChipClass() string {
	switch n.State {
	case NtfyStateOn:
		return "ok"
	case NtfyStateRefused:
		return "fail"
	default:
		return "dim"
	}
}

// Word is the chip's UPPERCASE mono word (the CSS uppercases; the value stays
// the copy's spelling).
func (n ntfyCardVM) Word() string {
	switch n.State {
	case NtfyStateOn:
		return "ON"
	case NtfyStateRefused:
		return "REFUSED"
	default:
		return "OFF"
	}
}

// ntfyTestStatus is the drawer's Test-verb status line: Class "ok" or "fail",
// and the message copy. Msg carries the whole ok line ("test delivered ·
// 12:24:51 · exocomp-7f3a") or the failure sentence ("the server refused the
// token"); the template adds the ✓/▲ glyph.
type ntfyTestStatus struct {
	Class string
	Msg   string
}

// ntfyVM is the /system/ntfy drawer (SPEC §2.2): the leg judgment, the sealed
// presence rows, the Detail two-position switch, the fixed fan-out table, the
// MUTES section (SPEC §7 A8), and the Test/Save verbs with their status line.
type ntfyVM struct {
	Page     page
	Operator bool
	Leg      ntfyCardVM
	Status   ntfyTestStatus
	Mutes    []muteRow
}

func (vm ntfyVM) HasStatus() bool { return vm.Status.Class != "" }

// muteRow is one active mute in the drawer's MUTES section (SPEC §7 A8): the
// source and host it covers, when it lifts, and the clear verb. Source is the
// alert name an alert-scoped mute covers and the word "global" for a global
// one; host is the host a host-scoped mute covers; both render "—" when the
// scope has no such dimension. A global mute is the safety valve — clearing
// it asks first, so ClearConfirm carries the confirm sentence, empty for a
// one-click clear.
type muteRow struct {
	ID           int64
	Source       string
	Host         string
	Until        string
	ClearConfirm string
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
	// Refusal is the row verb's refusal sentence, carried to the rowActions
	// partial by scheduleRowActions (forms/SPEC.md): non-empty only on a
	// refused row-verb re-render.
	Refusal string
}

func (r scheduleRow) LastClass() string { return stateClass(r.LastState) }

// BackupTarget is the backup/drill schedule's back-link target (A5): the
// stored Args.Target reference its fires operate on — the same reference the
// scheduler's fire-time concurrency keys backup:<target> / drill:<target>
// derive from (internal/sched). Empty for an every-target schedule and for
// kinds that carry no backup target (shell, play), which render no back-link
// to /system/backups.
func (r scheduleRow) BackupTarget() string {
	if (r.Kind == store.ScheduleBackup || r.Kind == store.ScheduleDrill) && r.Target != "" {
		return r.Target
	}
	return ""
}

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
	// LSWord is the self-care powerline's litestream segment word — the leg
	// plus its word ("litestream no sidecar", "litestream 4m"), never the
	// retired `rpo` metric name.
	LSWord string
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
	// Form is the add-source dialog's refusal state (the netgroup pattern):
	// nil at rest, the filled form with field errors on a 422 re-render.
	Form *sourceFormVM
}

// sourceFormVM is the add-source dialog's refusal state. ErrName and
// ErrLocation hold the human sentences (the raw validator words go to the log
// at the same call site); ErrForm is the whole-submit refusal for the status
// line (a duplicate, a missing sealer). FirstInvalid names the control to
// focus; Submitting drives the status line's "Adding" and disables the primary.
type sourceFormVM struct {
	Name         string
	Location     string
	Ref          string
	ErrName      string
	ErrLocation  string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *sourceFormVM) effective() sourceFormVM {
	if f == nil {
		return sourceFormVM{}
	}
	return *f
}

func (f *sourceFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *sourceFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// nameField and locationField build the reusable field's render state from the
// form's current values and refusals, mirroring the netgroup panel's fieldInput
// list.
func (f *sourceFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "src-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "homelab", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *sourceFormVM) locationField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "src-location", Label: "Path or git URL", Name: "location", Value: v.Location,
		Placeholder: "/abs/or/relative/path · git@host:org/repo.git · https://host/org/repo.git",
		Help:        "An absolute path, or a git@host / https://host clone URL.",
		ErrMsg:      v.ErrLocation, FirstInvalid: v.FirstInvalid == "location",
	}
}

type schedulesVM struct {
	Page      page
	Operator  bool
	Schedules []scheduleRow
	// Prefill carries the add form's deep-link values and Open flag;
	// Form is the add dialog's refusal state (nil at rest, the filled form
	// with field refusals on a 422 re-render). The field builders merge the
	// prefill into a fresh form so a deep link still lands filled.
	Prefill schedulePrefill
	Form    *scheduleFormVM
}

// scheduleFormVM is the schedule-add dialog's refusal state (forms/SPEC.md).
// Err* hold the translated sentences — the raw validator words go to the log
// at the same call site — and FixName is the one-click slug the fix link
// applies. FirstInvalid names the control to focus; Submitting drives the
// status line's verb and disables the primary.
type scheduleFormVM struct {
	Name                string
	Cron                string
	Kind                string
	Argv                string
	Target              string
	ConcurrencyKey      string
	Jitter              string
	Check               bool
	TolerateUnreachable bool
	ErrName             string
	FixName             string
	ErrCron             string
	ErrKind             string
	ErrArgv             string
	ErrTarget           string
	ErrConcurrency      string
	ErrJitter           string
	ErrForm             string
	FirstInvalid        string
	Submitting          bool
}

func (f *scheduleFormVM) effective() scheduleFormVM {
	if f == nil {
		return scheduleFormVM{}
	}
	return *f
}

func (f *scheduleFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *scheduleFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// prefillMerged returns the at-rest field value, falling back to the deep-link
// prefill only on a fresh form (a 422 re-render always echoes what was typed).
func (f *scheduleFormVM) prefillMerged(typed, pre string) string {
	if f != nil {
		return typed
	}
	if typed == "" {
		return pre
	}
	return typed
}

func (f *scheduleFormVM) nameField(p schedulePrefill) fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-name", Label: "Name", Name: "name", Value: f.prefillMerged(v.Name, p.Name),
		Placeholder: "nightly-backup", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *scheduleFormVM) cronField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-cron", Label: "Cron expression", Name: "cron", Value: v.Cron,
		Placeholder: "0 */5 * * * * or @every 5m",
		Help:        "A 5-field cron or an @every — like 0 */5 * * * * or @every 5m.",
		ErrMsg:      v.ErrCron, FirstInvalid: v.FirstInvalid == "cron",
	}
}

func (f *scheduleFormVM) argvField(p schedulePrefill) fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-argv", Label: "Command (shell/play kinds)", Name: "argv",
		Value:       f.prefillMerged(v.Argv, p.Argv),
		Placeholder: "shell: the command; play: playbook.yml [--extra-vars ...]",
		Help:        "The command (shell) or playbook command (play).",
		ErrMsg:      v.ErrArgv, FirstInvalid: v.FirstInvalid == "argv",
	}
}

func (f *scheduleFormVM) targetField(p schedulePrefill) fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-target", Label: "Target (backup/drill: target name, empty = every enabled target; play: standing inventory name)",
		Name: "target", Value: f.prefillMerged(v.Target, p.Target),
		Placeholder: "",
		Help:        "backup/drill: target name, empty = every target; play: inventory name.",
		ErrMsg:      v.ErrTarget, FirstInvalid: v.FirstInvalid == "target",
	}
}

func (f *scheduleFormVM) concurrencyField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-ck", Label: "Concurrency key (shell/play only)", Name: "concurrency_key",
		Value: v.ConcurrencyKey, Placeholder: "",
		Help:   "One run at a time per key.",
		ErrMsg: v.ErrConcurrency, FirstInvalid: v.FirstInvalid == "concurrency",
	}
}

func (f *scheduleFormVM) jitterField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "sc-jitter", Label: "Jitter seconds (optional)", Name: "jitter",
		Value: v.Jitter, Placeholder: "0",
		Help:   "A whole number of seconds — like 300.",
		ErrMsg: v.ErrJitter, FirstInvalid: v.FirstInvalid == "jitter",
	}
}

// kindIs reports whether the add form's kind select should show k: the typed
// value on a 422 re-render, else the deep-link prefill's kind, else the shell
// default.
func (f *scheduleFormVM) kindIs(p schedulePrefill, k string) bool {
	v := f.effective()
	if f == nil {
		if v.Kind == "" && p.Kind != "" {
			v.Kind = p.Kind
		}
		if v.Kind == "" {
			v.Kind = store.ScheduleShell
		}
	}
	return v.Kind == k
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
	// Form is the add-policy dialog's render state: nil at rest, the filled
	// form with field refusals on a 422 re-render. Notice is a whole-surface
	// refusal line (an rm that found nothing to remove) rendered above the
	// tray at 4xx — never a flash.
	Form   *policyFormVM
	Notice string
}

// policyFormVM is the add-policy dialog's refusal state (forms/SPEC.md).
// Err* hold the translated sentences — the raw validator words go to the log
// at the same call site — and FixName is the one-click slug the fix link
// applies. FirstInvalid names the control to focus; Submitting drives the
// status line's verb and disables the primary.
type policyFormVM struct {
	Name         string
	Workspace    string
	Action       string
	Expr         string
	ErrName      string
	FixName      string
	ErrWorkspace string
	ErrAction    string
	ErrExpr      string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *policyFormVM) effective() policyFormVM {
	if f == nil {
		return policyFormVM{}
	}
	return *f
}

func (f *policyFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *policyFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

func (f *policyFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "pl-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "no-destroy", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *policyFormVM) workspaceField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "pl-workspace", Label: "Workspace", Name: "workspace", Value: v.Workspace,
		Placeholder: "prod", Help: "The workspace this gate covers.",
		ErrMsg: v.ErrWorkspace, FirstInvalid: v.FirstInvalid == "workspace",
	}
}

func (f *policyFormVM) exprField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "pl-expr", Label: "CEL expression", Name: "expr", Value: v.Expr,
		Placeholder: `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`,
		Help:        "A CEL expression over plan.",
		ErrMsg:      v.ErrExpr, FirstInvalid: v.FirstInvalid == "expr",
	}
}

// actionIs reports whether the add form's action select should show k: the
// typed value on a 422 re-render, the deny default at rest.
func (f *policyFormVM) actionIs(k string) bool {
	v := f.effective()
	if v.Action == "" {
		return k == "deny"
	}
	return v.Action == k
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
	// Form is the add-runner dialog's render state: nil at rest, the filled
	// form with field refusals on a 422 re-render. Notice is a whole-surface
	// refusal line (an rm that found nothing to remove) rendered above the
	// tray at 4xx — never a flash.
	Form   *runnerFormVM
	Notice string
}

// runnerFormVM is the add-runner dialog's refusal state (forms/SPEC.md).
// Err* hold the translated sentences — the raw validator words go to the log
// at the same call site — and FixName is the one-click slug the fix link
// applies. FirstInvalid names the control to focus; Submitting drives the
// status line's verb and disables the primary.
type runnerFormVM struct {
	Name             string
	Mode             string
	Base             string
	Bootstrap        string
	Containerfile    string
	Packages         string
	AnsibleCore      string
	MemoryMax        string
	CPUQuota         string
	ErrName          string
	FixName          string
	ErrMode          string
	ErrBase          string
	ErrBootstrap     string
	ErrContainerfile string
	ErrForm          string
	FirstInvalid     string
	Submitting       bool
}

func (f *runnerFormVM) effective() runnerFormVM {
	if f == nil {
		return runnerFormVM{}
	}
	return *f
}

func (f *runnerFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *runnerFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

func (f *runnerFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "fedora-ee",
		Help:        "A lowercase letter or digit, then letters, digits, dots, hyphens, underscores.",
		ErrMsg:      v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *runnerFormVM) baseField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-base", Label: "Base image", Name: "base", Value: v.Base,
		Placeholder: "registry.fedoraproject.org/fedora-minimal:44",
		Help:        "A base image reference, like registry.fedoraproject.org/fedora-minimal:44.",
		ErrMsg:      v.ErrBase, FirstInvalid: v.FirstInvalid == "base",
	}
}

func (f *runnerFormVM) containerfileField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-containerfile", Label: "Containerfile path (Model B; excludes bootstrap)",
		Name: "containerfile", Value: v.Containerfile, Placeholder: "",
		Help:   "A Containerfile path in the source (Model B).",
		ErrMsg: v.ErrContainerfile, FirstInvalid: v.FirstInvalid == "containerfile",
	}
}

func (f *runnerFormVM) packagesField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-packages", Label: "Packages (comma-separated)", Name: "packages",
		Value: v.Packages, Placeholder: "sshpass, rsync",
		Help: "Comma-separated.",
	}
}

func (f *runnerFormVM) ansibleField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-ansible-core", Label: "ansible-core version pin", Name: "ansible_core",
		Value: v.AnsibleCore, Placeholder: "",
		Help: "A version pin, like 2.18.1.",
	}
}

func (f *runnerFormVM) memoryField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-memory-max", Label: "Memory ceiling", Name: "memory_max",
		Value: v.MemoryMax, Placeholder: "2G",
		Help: "A size, like 2G.",
	}
}

func (f *runnerFormVM) cpuField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rn-cpu-quota", Label: "CPU ceiling", Name: "cpu_quota",
		Value: v.CPUQuota, Placeholder: "200%",
		Help: "A percentage, like 200%.",
	}
}

// modeIs reports whether the add form's mode select should show k: the typed
// value on a 422 re-render, the cached default at rest.
func (f *runnerFormVM) modeIs(k string) bool {
	v := f.effective()
	if v.Mode == "" {
		return k == "cached"
	}
	return v.Mode == k
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
	// FocusKey is the ?key= deep-link value (SPEC §7 A2): the sealed secret
	// whose row the page focuses — the target of m75's Seal-token href
	// /secrets?key=ntfy/token. Empty renders no row focused.
	FocusKey string
	// Form is the add-secret dialog's refusal state (the netgroup pattern):
	// nil at rest, the filled form with field errors on a 422 re-render.
	Form *secretAddFormVM
	// Rotate is the rotate dialog's refusal state: nil at rest, else the one
	// refused row's form, which the 422 re-render re-opens.
	Rotate *secretRotateFormVM
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
	// UsedBy lists the /system legs that read this secret (SPEC §7 A2),
	// empty when no leg consumes it.
	UsedBy []usedByRow
	// Refusal is the rm verb's refusal sentence, carried to the rowActions
	// partial by secretRowActions (forms/SPEC.md): non-empty only on a refused
	// rm re-render. The sealed material stays absent — invariant 4.
	Refusal string
}

// secretBindingRow is one run-secret binding that injects a secret and the env
// var name it maps the value to at execution time. It carries no value.
type secretBindingRow struct {
	Binding string
	EnvName string
}

// usedByRow is one /system leg that reads a sealed secret (SPEC §7 A2 "who
// uses this"): the leg's name and the page it lives on, so a rotation names
// its consumer. Only keys a leg actually consumes get a row.
type usedByRow struct {
	Name string
	Href string
}

// expiryMarker is a secret's staleness state rendered as a labelled chip: an
// expired secret is a loud failure, an approaching expiry a wait, a distant or
// absent one dim. Never colour-only — the label always names the state.
type expiryMarker struct {
	Label string // EXPIRED | expires in Nd | no expiry
	Class string // fail | wait | dim
}

// secretAddFormVM is the seal-a-secret dialog's refusal state (the netgroup
// pattern). Name, Kind, and Expires are the non-secret typed values a 422
// re-render keeps; the secret value and confirm_token are never carried back —
// the value field renders empty always, so a refusal can never echo the
// plaintext (invariant 4: sealed = presence, never value). ErrForm is the
// whole-submit refusal for the status line; FirstInvalid names the control to
// focus; Submitting drives the status line and disables the primary.
type secretAddFormVM struct {
	Name         string
	Kind         string
	Expires      string
	ErrName      string
	ErrExpires   string
	ErrValue     string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *secretAddFormVM) effective() secretAddFormVM {
	if f == nil {
		return secretAddFormVM{}
	}
	return *f
}

func (f *secretAddFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *secretAddFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

func (f *secretAddFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "se-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "deploy-token", Help: "Letters, digits, dots, hyphens, underscores, slashes.",
		ErrMsg: v.ErrName, FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *secretAddFormVM) kindField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "se-kind", Label: "Kind", Name: "kind", Value: v.Kind,
		Placeholder: "generic", Help: "What kind of value this is — e.g. generic, token.",
		FirstInvalid: v.FirstInvalid == "kind",
	}
}

// valueField is the sealed-value control: Value is always empty — the plaintext
// lives in the operator's clipboard, never in re-rendered HTML (invariant 4).
func (f *secretAddFormVM) valueField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "se-val", Label: "Value", Name: "value", Value: "",
		Help:   "Sealed into the envelope — never shown again.",
		ErrMsg: v.ErrValue, FirstInvalid: v.FirstInvalid == "value",
	}
}

func (f *secretAddFormVM) expiresField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "se-exp", Label: "Expires (optional)", Name: "expires", Value: v.Expires,
		Placeholder: "never", Help: "A duration — e.g. 720h, 30d.",
		ErrMsg: v.ErrExpires, FirstInvalid: v.FirstInvalid == "expires",
	}
}

// secretRotateFormVM is the rotate dialog's refusal state, one per refused row.
// Name is the target secret so the 422 re-render re-opens the right dialog.
// Expires is the only non-secret typed value kept; the new value never
// re-renders (invariant 4).
type secretRotateFormVM struct {
	Name         string
	Expires      string
	ErrValue     string
	ErrExpires   string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *secretRotateFormVM) effective() secretRotateFormVM {
	if f == nil {
		return secretRotateFormVM{}
	}
	return *f
}

// rotateForm dereferences the rotate refusal state for one row: the refused
// form when the row is the target, else an at-rest form for that name.
func (vm secretsVM) rotateForm(name string) secretRotateFormVM {
	if vm.Rotate != nil && vm.Rotate.Name == name {
		return *vm.Rotate
	}
	return secretRotateFormVM{Name: name}
}

func (vm secretsVM) rotatePending() bool {
	return vm.Rotate != nil && vm.Rotate.Submitting
}

func (vm secretsVM) rotateErrForm() string {
	if vm.Rotate == nil {
		return ""
	}
	return vm.Rotate.ErrForm
}

func (vm secretsVM) rotateValueField(name string) fieldMeta {
	f := vm.rotateForm(name)
	return fieldMeta{
		ID: "sv-" + name, Label: "New value", Name: "value", Value: "",
		Placeholder: "new value", Help: "Sealed into the envelope — never shown again.",
		ErrMsg: f.ErrValue, FirstInvalid: f.FirstInvalid == "value",
	}
}

func (vm secretsVM) rotateExpiresField(name string) fieldMeta {
	f := vm.rotateForm(name)
	return fieldMeta{
		ID: "se-exp-" + name, Label: "Expires (optional)", Name: "expires", Value: f.Expires,
		Placeholder: "keep expiry", Help: "A duration — e.g. 720h, 30d.",
		ErrMsg: f.ErrExpires, FirstInvalid: f.FirstInvalid == "expires",
	}
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
	Enabled   bool
	State     string
	Snapshot  string
	Age       string
	Freshness string // ok|wait|fail per 24h/48h thresholds
	Drill     bool
	// RunningJobID is the job whose backup run is in flight, 0 otherwise. The
	// card draws A9's `● RUNNING · job #…` from it; the run's job is the newest
	// for the target's "backup:<name>" concurrency key, because the job opens
	// the run the moment it is claimed.
	RunningJobID int64
}

// backupsVM is the /system/backups management surface (SPEC §4.1): every
// target with its retention, schedule and last run, the Add-target dialog,
// and the Restore DR drawer. AddForm is the add dialog's render state (nil at
// rest, the filled form with refusals on a 422 re-render); RetentionForm is
// one row's edit-retention dialog state (nil at rest, else the one refused
// row's form, which the 422 re-render re-opens); Restore is the DR drawer's
// state (nil at rest, the filled drawer on a 422 re-render, or the opened
// drawer for a deep link). Snapshots is the cross-target snapshot read the
// drawer's radios draw (fold S-3). Notice is a whole-surface refusal line (an
// rm that found nothing to remove) rendered above the tray at 4xx — never a
// flash.
type backupsVM struct {
	Page          page
	Operator      bool
	Targets       []backupTargetRow
	Snapshots     []snapshotRow
	AddForm       *backupAddFormVM
	RetentionForm *backupRetentionFormVM
	Restore       *restoreFormVM
	Notice        string
}

// backupsHistoryVM is the per-target snapshot history page (SPEC §7 A4/A12):
// the one page where a snapshot row's ▾ holds forget. Target names the target
// the page belongs to; Snapshots are its rows, newest first. Notice is a
// whole-surface refusal line (a forget that named no row) rendered above the
// tray at 4xx — never a flash.
type backupsHistoryVM struct {
	Page      page
	Operator  bool
	Target    string
	Kind      string
	Enabled   bool
	Snapshots []historySnapRow
	Notice    string
}

// historySnapRow is one recoverable point in time on a target's history page
// (SPEC §7 A12): a SUCCEEDED run's snapshot, newest first. ID is the short id
// the row renders; FullID the stored id the forget form posts. Drilled marks
// the newest snapshot of a drilled target and carries the drill's outcome
// (A4's history face). Forget is the forget verb's gate: it renders only on a
// forgettable row — never the newest, never a drilled one. Refusal is the
// row's forget-refusal sentence on a 422 re-render.
type historySnapRow struct {
	ID            string
	FullID        string
	Age           string
	Size          string
	Newest        bool
	Drilled       bool
	DrillAge      string
	DrillBytes    string
	DrillDuration string
	Forget        bool
	Refusal       string
}

// DrillFacts is the drilled row's outcome line on the history page (A4): the
// restored bytes and duration, empty when the drill job left no parseable
// outcome.
func (r historySnapRow) DrillFacts() string {
	if r.DrillBytes == "" {
		return ""
	}
	if r.DrillDuration == "" {
		return r.DrillBytes + " verified"
	}
	return r.DrillBytes + " verified · " + r.DrillDuration
}

// backupTargetRow is one row of the /system/backups tray table (SPEC §4.1):
// its posture (TARGET · KIND · RETENTION · SCHEDULE) and the newest run's
// facts (LAST RUN). URI is scrubbed of userinfo for the KIND cell — a target
// whose --uri embeds credentials must not echo them (invariant 4).
type backupTargetRow struct {
	Name      string
	Kind      string
	URI       string
	Retention string
	Enabled   bool
	Base      string
	// Schedule facts: Cadence is the target's kind=backup schedule cron ("" =
	// none) and the add dialog's prefill source; Next is the human "next in
	// {t}" line.
	Cadence string
	Next    string
	// Last-run facts.
	HasRun   bool
	State    string // running|succeeded|failed|cancelled|never
	Snapshot string
	Age      string
	Bytes    string
	Duration string
	JobID    int64
	// DrillOK/DrillAge read the target's newest backup-drill job: a succeeded
	// drill marks the newest snapshot "drilled {age} ago ok" (A4's data).
	// DrillBytes/DrillDuration are the drill's own outcome — the restored
	// bytes and wall time parsed from its "restore drill OK" event — empty
	// when the job left no parseable line (A4).
	DrillOK       bool
	DrillAge      string
	DrillBytes    string
	DrillDuration string
	// FailedSeq counts consecutive failed runs, the "{n} failed in a row"
	// fact of a FAILED row.
	FailedSeq int
	Reason    string
	// Refusal is a row verb's refusal sentence (forms/SPEC.md), carried to the
	// rowActions partial by backupRowActions: non-empty only on a refused
	// row-verb re-render.
	Refusal string
}

// RetentionLabel is the RETENTION cell's first line: the parsed policy words
// ("daily 7 · weekly 4", "keep all") or "—" for a git target.
func (r backupTargetRow) RetentionLabel() string {
	if r.Kind == backup.KindGit {
		return "—"
	}
	switch r.Retention {
	case "none":
		return "keep all"
	case "":
		return "daily 7 · weekly 4"
	}
	parts := strings.Split(r.Retention, ",")
	words := make([]string, 0, len(parts))
	for _, p := range parts {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			words = append(words, k+" "+v)
		}
	}
	if len(words) == 0 {
		return r.Retention
	}
	return strings.Join(words, " · ")
}

// RetentionMeta is the RETENTION cell's second line: the one honest
// consequence ("prunes after run", "never prunes · grows", "git keeps
// history").
func (r backupTargetRow) RetentionMeta() string {
	if r.Kind == backup.KindGit {
		return "git keeps history"
	}
	if r.Retention == "none" {
		return "never prunes · grows"
	}
	return "prunes after run"
}

// RetentionClass names the RETENTION cell's signal: unbounded growth ("keep
// all") is a wait finding, the pruned policies quiet, a git target dim.
func (r backupTargetRow) RetentionClass() string {
	if r.Retention == "none" {
		return "wait"
	}
	if r.Kind == backup.KindGit {
		return "dim"
	}
	return "ok"
}

// HasSchedule reports whether the target carries a kind=backup schedule (the
// SCHEDULE cell's cadence line and its "add one" fallback).
func (r backupTargetRow) HasSchedule() bool { return r.Cadence != "" }

// StateClass names the LAST RUN state word's chip colour, mirroring the job
// palette: ok for succeeded, fail for failed/cancelled, run for running, dim
// for never.
func (r backupTargetRow) StateClass() string {
	switch r.State {
	case "succeeded":
		return "ok"
	case "failed":
		return "fail"
	case "running":
		return "run"
	case "cancelled":
		return "fail"
	default:
		return "dim"
	}
}

// LastRunWord is the LAST RUN cell's chip word, "" for a never-run target
// (which renders as plain dim text, not a chip).
func (r backupTargetRow) LastRunWord() string {
	if !r.HasRun {
		return ""
	}
	return strings.ToUpper(r.State)
}

// LastRunLine is the LAST RUN cell's first line after the chip: the snapshot
// and age for a success, the age for a failure, the elapsed for a running run
// (the job link renders beside it, R7's "only job # links"), "never ran" for
// a target with no history.
func (r backupTargetRow) LastRunLine() string {
	switch r.State {
	case "running":
		return r.Duration
	case "succeeded":
		return "#" + r.Snapshot + " " + r.Age + " ago"
	case "failed":
		return r.Age + " ago"
	case "cancelled":
		return r.Age + " ago"
	default:
		return "never ran"
	}
}

// LastRunMeta is the LAST RUN cell's second line (R7): it LEADS with the drill
// slot — `drill —` when never drilled, `drill {age} ago ok` + the drill's
// outcome (A4) when the newest drill succeeded — then the run's own bytes and
// duration. The job id is not in the string: the template renders it as its
// own link so only `job #N` reads as a link, never the whole line.
func (r backupTargetRow) LastRunMeta() string {
	switch r.State {
	case "succeeded":
		parts := []string{}
		if r.DrillOK {
			parts = append(parts, "drill "+r.DrillAge+" ago ok")
		} else {
			parts = append(parts, "drill —")
		}
		if fact := r.DrillFact(); fact != "" {
			parts = append(parts, fact)
		}
		if r.Bytes != "" {
			parts = append(parts, "+"+r.Bytes)
		}
		if r.Duration != "" {
			parts = append(parts, r.Duration)
		}
		return strings.Join(parts, " · ")
	case "failed":
		parts := []string{}
		if r.Reason != "" {
			parts = append(parts, truncateMeta(r.Reason, lastRunReasonCap))
		}
		if r.FailedSeq > 1 {
			parts = append(parts, itoa(int64(r.FailedSeq))+" failed in a row")
		}
		return strings.Join(parts, " · ")
	case "cancelled":
		return ""
	}
	return ""
}

// DrillFact is the LAST RUN second line's drill-outcome fragment (SPEC §7
// A4): the restored bytes and the drill's duration, empty when the drill job
// left no parseable outcome.
func (r backupTargetRow) DrillFact() string {
	if r.DrillBytes == "" {
		return ""
	}
	if r.DrillDuration == "" {
		return r.DrillBytes + " verified"
	}
	return r.DrillBytes + " verified · " + r.DrillDuration
}

// backupsCardChip derives the Backups card's state chip from the freshness
// rows the card draws (copy.txt): OK (ok), {n} OF {m} FAILING (wait; fail when
// n ≥ 2 or n = m), NONE (wait) when no ENABLED target exists or one has never
// run. A DISABLED target is not a failure and not a gap — it is skipped, so an
// OK target keeps the chip OK (R4).
func backupsCardChip(rows []backupRow) (string, string) {
	nFailed, nNever := 0, 0
	nEnabled := 0
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		nEnabled++
		switch r.State {
		case "failed":
			nFailed++
		case "never":
			nNever++
		}
	}
	switch {
	case nFailed > 0:
		if nFailed >= 2 || nFailed == nEnabled {
			return itoa(int64(nFailed)) + " OF " + itoa(int64(nEnabled)) + " FAILING", "fail"
		}
		return "1 OF " + itoa(int64(nEnabled)) + " FAILING", "wait"
	case nEnabled == 0 || nNever > 0:
		return "NONE", "wait"
	default:
		return "OK", "ok"
	}
}

// retentionCells splits a stored retention spec into the edit dialog's cells
// (daily · weekly · keep all). "none" is keep-all; an unparseable spec leaves
// the cells empty and the meta shows the raw value.
func retentionCells(spec string) (daily, weekly string, keepAll bool) {
	if spec == "none" {
		return "", "", true
	}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			switch k {
			case "daily":
				daily = v
			case "weekly":
				weekly = v
			}
		}
	}
	return daily, weekly, false
}

// retentionFormFor dereferences one row's edit-retention dialog state, filling
// the at-rest form from the row's stored retention so the modal opens with the
// current policy.
func (vm backupsVM) retentionFormFor(row backupTargetRow) backupRetentionFormVM {
	f := vm.retentionForm(row.Name)
	if vm.RetentionForm == nil || vm.RetentionForm.Name != row.Name {
		f.Daily, f.Weekly, f.KeepAll = retentionCells(row.Retention)
	}
	return f
}

// NewestAge is the drawer's From meta "snapshot · {age} old": the newest
// snapshot's age across targets.
func (f *restoreFormVM) NewestAge() string {
	if len(f.Snapshots) == 0 {
		return "—"
	}
	return f.Snapshots[0].Age
}

// snapshotRow is one recoverable point in time in the Restore drawer's radios
// (SPEC §4.3, fold S-3): a SUCCEEDED run's snapshot across ANY target, newest
// first. ID is the short snapshot id the typed confirm must match (the CLI's
// shortID length). Drilled marks the newest snapshot of a target whose newest
// drill job succeeded — a drill verifies the newest snapshot at drill time
// (A4's data source), so only that one is proven.
type snapshotRow struct {
	ID       string
	Target   string
	Age      string
	Size     string
	Drilled  bool
	DrillAge string
}

// restoreFormVM is the Restore DR drawer's render state (SPEC §4.3). Mode is
// the From switch: "snapshot" (the DR path) or "replica" (litestream mode —
// m76's restore-from-replica deep-links the drawer with Mode=replica, the
// same form and the same typed-confirm gate, where the typed confirm matches
// the word "replica"). Open reports whether the drawer should auto-open.
// Snapshots are the radios' rows; Selected is the checked radio's snapshot
// id; Confirm is the typed id. LossAge and Waiting name the WHAT HAPPENS loss
// for the selected snapshot (its age and the pending approvals that would
// die); RunningJobs is the "the queue drains · {n} running jobs finish"
// count; Lag is the replica meta for the From switch. ErrConfirm is the hard
// rule (c) refusal — a typed id that does not match the selected snapshot —
// and ErrForm a whole-submit refusal.
type restoreFormVM struct {
	Mode        string
	Open        bool
	Snapshots   []snapshotRow
	Selected    string
	Confirm     string
	LossAge     string
	Waiting     int
	RunningJobs int
	Lag         string
	ErrConfirm  string
	ErrForm     string
	Submitting  bool
	// Confirmed is true once the typed id has matched: the drawer body then
	// changes to the RUN ON THE HOST block — the recovery-kit command with its
	// Copy verb — instead of the confirm form. JS reveals it in place; the
	// server sets it on a JS-off valid submit (D3).
	Confirmed bool
}

func (f *restoreFormVM) effective() restoreFormVM {
	if f == nil {
		return restoreFormVM{}
	}
	return *f
}

// SelectedSnapshot returns the radio the operator picked, nil when nothing is
// selected — the source of the danger button's label and the loss line.
func (f *restoreFormVM) SelectedSnapshot() *snapshotRow {
	for i := range f.Snapshots {
		if f.Snapshots[i].ID == f.Selected {
			return &f.Snapshots[i]
		}
	}
	return nil
}

// WaitingLabel is the WHAT HAPPENS loss count, pluralised the way the copy
// reads ("1 waiting approval" · "2 waiting approvals").
func (f *restoreFormVM) WaitingLabel() string {
	if f.Waiting == 1 {
		return "1 waiting approval"
	}
	return itoa(int64(f.Waiting)) + " waiting approvals"
}

// Pending reports whether the drawer is mid-submit, disabling the primary.
func (f *restoreFormVM) Pending() bool { return f != nil && f.Submitting }

// ErrFormText is the whole-submit refusal for the status line.
func (f *restoreFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// backupAddFormVM is the Add-target dialog's refusal state (forms/SPEC.md).
// Name, Kind, Repository, Daily, Weekly, KeepAll and ScheduleCron are the
// typed values a 422 re-render echoes. The repository password is never
// carried or rendered — it is derived on the daemon side when the first
// backup runs (invariant 4), so the form has no password field at all.
// Sibling names the newest existing target whose cadence prefilled
// ScheduleCron ("" when none). Err* hold the human sentences; ErrForm the
// whole-submit refusal; FirstInvalid names the control to focus; Submitting
// drives the status line and disables the primary.
type backupAddFormVM struct {
	Name         string
	Kind         string
	Repository   string
	Daily        string
	Weekly       string
	KeepAll      bool
	ScheduleCron string
	Sibling      string
	ErrName      string
	FixName      string
	ErrKind      string
	ErrRepo      string
	ErrRetention string
	ErrSchedule  string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *backupAddFormVM) effective() backupAddFormVM {
	if f == nil {
		return backupAddFormVM{}
	}
	return *f
}

func (f *backupAddFormVM) Pending() bool { return f != nil && f.Submitting }

func (f *backupAddFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// kindIs reports whether the Kind segmented switch shows k: the typed value on
// a 422 re-render, the restic_local default at rest.
func (f *backupAddFormVM) kindIs(k string) bool {
	v := f.effective()
	if v.Kind == "" {
		return k == backup.KindResticLocal
	}
	return v.Kind == k
}

func (f *backupAddFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "bk-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "nas01",
		Help:        "How the dead-man, the powerline and the recovery kit will name it.",
		ErrMsg:      v.ErrName, FixValue: v.FixName,
		FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *backupAddFormVM) repoField() fieldMeta {
	v := f.effective()
	help := "restic -r syntax · the repository password is derived on the daemon side, never stored or shown."
	if v.Kind == backup.KindGit {
		help = "A git remote — restic kinds take a repository, git takes a remote."
	}
	return fieldMeta{
		ID: "bk-repo", Label: "Repository", Name: "repository", Value: v.Repository,
		Placeholder: "local:/var/backups/exocomp · s3:s3.us-west-002.backblazeb2.com/exocomp · git@forge.lan:ops/exocomp-config.git",
		Help:        help,
		ErrMsg:      v.ErrRepo, FirstInvalid: v.FirstInvalid == "repository",
	}
}

func (f *backupAddFormVM) scheduleField() fieldMeta {
	v := f.effective()
	help := "Creates a kind=backup schedule on /schedules · edit it there."
	if v.Sibling != "" {
		help = "Same cadence as " + v.Sibling + " · creates a kind=backup schedule on /schedules · edit it there."
	}
	return fieldMeta{
		ID: "bk-schedule", Label: "Schedule", Name: "schedule", Value: v.ScheduleCron,
		Placeholder: "0 2 * * * * or @every 24h · empty = no schedule",
		Help:        help,
		ErrMsg:      v.ErrSchedule, FirstInvalid: v.FirstInvalid == "schedule",
	}
}

// RetentionValue renders the retention cells into the stored spec: "none"
// for keep-all, the "daily=d,weekly=w" form from whichever cells are set
// (both empty is the default policy — a week of dailies and a month of
// weeklies, which the engine applies when the spec is empty).
func (f *backupAddFormVM) RetentionValue() string {
	v := f.effective()
	return retentionSpec(v.Daily, v.Weekly, v.KeepAll)
}

// backupRetentionFormVM is one row's edit-retention dialog state: the one
// refused row's cells on a 422 re-render, so the modal re-opens filled.
// KeepAll maps the "never prunes · grows" (--retention none) case.
type backupRetentionFormVM struct {
	Name         string
	Daily        string
	Weekly       string
	KeepAll      bool
	ErrDaily     string
	ErrWeekly    string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

// retentionForm dereferences the edit-retention state for one row: the
// refused form when the row is the target, else an at-rest form for that name.
func (vm backupsVM) retentionForm(name string) backupRetentionFormVM {
	if vm.RetentionForm != nil && vm.RetentionForm.Name == name {
		return *vm.RetentionForm
	}
	return backupRetentionFormVM{Name: name}
}

func (f backupRetentionFormVM) effective() backupRetentionFormVM {
	return f
}

func (f backupRetentionFormVM) Pending() bool { return f.Submitting }

func (f backupRetentionFormVM) ErrFormText() string {
	return f.ErrForm
}

// RetentionValue renders the edit-retention cells into the stored spec, the
// mirror of backupAddFormVM.RetentionValue.
func (f *backupRetentionFormVM) RetentionValue() string {
	v := f.effective()
	return retentionSpec(v.Daily, v.Weekly, v.KeepAll)
}

// retentionSpec builds the stored retention string from the edit dialog's
// cells: "none" for keep-all, the set cells joined, "" for the default.
func retentionSpec(daily, weekly string, keepAll bool) string {
	if keepAll {
		return "none"
	}
	var parts []string
	if daily != "" {
		parts = append(parts, "daily="+daily)
	}
	if weekly != "" {
		parts = append(parts, "weekly="+weekly)
	}
	return strings.Join(parts, ",")
}

// workspaceRow is one tofu workspace with its drift marker.
type workspaceRow struct {
	Name    string
	Serial  int64
	Drift   bool
	LastJob string
}

// stateTrayRow is one stored tfstate serial of the workspace detail's Tofu
// state tray (SPEC §2.3): the serial, when it was written, and who wrote it —
// a job link, "outside exocomp", or the adoption line for the FIRST serial.
// BrowseID is the saved-plan id whose sealed `tofu-plan-show` companion renders
// the serial's show-json diff; 0 means no companion, so the row reads a "no
// sealed diff" note instead of a browse verb.
type stateTrayRow struct {
	Serial   int64
	When     string
	JobID    int64
	Adoption bool
	BrowseID int64
}

// Browseable reports whether this serial has a sealed show-json companion to
// browse to.
func (r stateTrayRow) Browseable() bool { return r.BrowseID != 0 }

// TileDir is the §2.1 identity grid's Dir tile: the workspace's tofu root, or
// a dash for the state-only shape (no dir to run from).
func (vm workspaceDetailVM) TileDir() string {
	if vm.Row.StateOnly() {
		return "—"
	}
	return vm.Row.TofuDir
}

// TileBackend is the §2.1 Backend tile: the recorded state-backend address,
// or a "not recorded" line before the daemon has one.
func (vm workspaceDetailVM) TileBackend() string {
	if vm.Config.Address == "" {
		return "not recorded"
	}
	return vm.Config.Address
}

// TileProvider is the §2.1 Provider mirror tile: online, or "offline · mirror
// only" in wait when the workspace resolves providers from the local mirror.
func (vm workspaceDetailVM) TileProvider() string {
	if vm.ProviderOffline {
		return "offline · mirror only"
	}
	return "online"
}

// TileProviderWait reports the provider-mirror tile's wait state (offline).
func (vm workspaceDetailVM) TileProviderWait() bool { return vm.ProviderOffline }

// TileState is the §2.1 Tofu state tile: the stored serial count, the value
// whose link lands on the tray card below.
func (vm workspaceDetailVM) TileState() string {
	return itoa(int64(len(vm.StateTray))) + " serials"
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
	// ScheduleForm is the Schedule dialog's refusal state (the netgroup
	// pattern): nil at rest, the filled form on a 422 re-render. ScheduleOpen
	// renders the dialog open for the JS-off fallback, mirroring the source
	// add dialog's vm.Form.
	ScheduleForm *jobScheduleForm
	ScheduleOpen bool
	// PinState is the Output pin's server-set data-state (§2): "complete" on a
	// terminal page whose event list carries a terminal band, "cut" on a
	// terminal page without one (an interrupted/lost runner), "following" on a
	// live page. JS only moves the chip between following and paused (and the
	// done flash), so a terminal page never reads FOLLOWING TAIL without JS.
	PinState string
	// EndedClock is the terminal run's finished HH:MM:SS clock for the meta's
	// "ended {hh:mm:ss}"; empty on a live job or one with no finished_at.
	EndedClock string
	// Findings are a kind=doctor job's self-check rows (SPEC §7 A1): one per
	// subsystem entry, parsed from the job's structured finding events. Empty
	// on every other kind.
	Findings []doctorFindingRow
}

// doctorFindingRow is one self-check finding on a doctor job's page: the
// subsystem, its status word, and the detail line — the same wire.HealthEntry
// `exocomp doctor --output json` emits, carried to the page as the event's
// structured payload. Class is the status's signal colour.
type doctorFindingRow struct {
	Name   string
	Status string
	Detail string
	Class  string
}

// doctorFindingClass maps a finding's status onto the chip palette: ok quiet,
// warn wait, fail loud, unknown dim — unknown is a state, not a fault.
func doctorFindingClass(status string) string {
	switch status {
	case wire.HealthOK:
		return "ok"
	case wire.HealthWarn:
		return "wait"
	case wire.HealthFail:
		return "fail"
	default:
		return "dim"
	}
}

// Word renders the finding's status the way chips read it: the uppercase word.
func (f doctorFindingRow) Word() string { return strings.ToUpper(f.Status) }

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
	// ErrLimit is the Limit field's refusal (the forms pattern): empty at rest,
	// the ▲ sentence on a 422 re-render that keeps the typed value.
	ErrLimit string
}

// limitField builds the Limit control's render state (the forms pattern): the
// field's help at rest, the refusal sentence on a 422 re-render.
func (r jobRerunVM) limitField() fieldMeta {
	return fieldMeta{
		ID: "jr-limit", Label: "Limit", Name: "limit", Value: r.Limit,
		Placeholder: "host or group pattern",
		Help:        "ansible --limit pattern — empty means all hosts.",
		ErrMsg:      r.ErrLimit, FirstInvalid: r.ErrLimit != "",
	}
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

// jobScheduleForm is the job-detail Schedule dialog's refusal state (the
// netgroup pattern): nil at rest, the filled form with field errors on a 422
// re-render. ErrName/ErrCron hold the human sentences (the raw validator words
// go to the log at the same call site); ErrForm is the whole-submit refusal
// for the status line. FirstInvalid names the control to focus; Submitting
// drives the status line's "Adding schedule" and disables the primary.
type jobScheduleForm struct {
	Name         string
	Cron         string
	ErrName      string
	ErrCron      string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *jobScheduleForm) effective() jobScheduleForm {
	if f == nil {
		return jobScheduleForm{}
	}
	return *f
}

func (f *jobScheduleForm) Pending() bool {
	return f != nil && f.Submitting
}

func (f *jobScheduleForm) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// nameField and cronField build the reusable field's render state from the
// form's current values and refusals, mirroring the source form's fieldInput
// list.
func (f *jobScheduleForm) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "js-name", Label: "Name", Name: "name", Value: v.Name,
		Placeholder: "nightly", Help: "Letters, digits, dots, hyphens, underscores.",
		ErrMsg: v.ErrName, FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *jobScheduleForm) cronField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "js-cron", Label: "Cron expression", Name: "cron", Value: v.Cron,
		Placeholder: "0 */5 * * * * or @every 5m",
		Help:        "Six fields with seconds first, or @every 5m / @daily.",
		ErrMsg:      v.ErrCron, FirstInvalid: v.FirstInvalid == "cron",
	}
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
	// Kill is the global-kill form's refusal state (the forms pattern): nil at
	// rest, the filled form on a 422 re-render with the kill details open.
	Kill *jobKillForm
}

// jobKillForm is the approval surface's "Silence & global kill" form refusal
// state: the typed expiry, the field's sentence (ErrExpiry), and the status
// line's whole-submit sentence (ErrForm — the hold-to-confirm gesture).
type jobKillForm struct {
	Expiry       string
	ErrExpiry    string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *jobKillForm) effective() jobKillForm {
	if f == nil {
		return jobKillForm{}
	}
	return *f
}

func (f *jobKillForm) Pending() bool {
	return f != nil && f.Submitting
}

func (f *jobKillForm) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// expiryField builds the expiry control's render state (the forms pattern):
// the field's help at rest, the refusal sentence on a 422 re-render.
func (f *jobKillForm) expiryField() fieldMeta {
	v := f.effective()
	err := ""
	if f != nil {
		err = f.ErrExpiry
	}
	return fieldMeta{
		ID: "kill-expiry", Label: "Expiry", Name: "expiry", Value: v.Expiry,
		Placeholder: "2h", Help: "A silence always expires — like 2h or 45m.",
		ErrMsg: err, FirstInvalid: f != nil && f.FirstInvalid == "expiry",
	}
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

// outcomeOrder is the six outcome chips' funnel order — the closed enum, all
// positions always present so a fixed order teaches where each word lives.
var outcomeOrder = []string{"ignored", "matched", "suppressed", "gated", "fired", "error"}

// eventsVM is the events screen: the last-24h funnel card, the filter row
// (source switch + outcome chips, always the closed enum), and the tray table
// of received alerts with the stored payload and evaluation trace inline.
// Source is the active source split ("" all · "checks" · "alerts"); Outcome
// the active chip ("" none or one of the six). Chips count the rows in view
// per split and outcome; Funnel is the 24h window the footer reads. Empty
// hides the tray (the W5 copy) — the filter row stays, the enum never hides.
type eventsVM struct {
	Page    page
	Funnel  store.EventOutcomeCounts
	Rows    []eventRow
	Chips   eventsChips
	Source  string
	Outcome string
	Empty   bool
}

// ChipCount is the count the chip or segment named key carries — the rows in
// view that share its outcome or source split.
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

// FunnelCount is the last-24h count of one outcome — the footer's "{count}
// {outcome}", from the same windowed funnel the card renders, never the tray
// rows.
func (vm eventsVM) FunnelCount(o string) int {
	switch o {
	case "ignored":
		return vm.Funnel.Ignored
	case "matched":
		return vm.Funnel.Matched
	case "suppressed":
		return vm.Funnel.Suppressed
	case "gated":
		return vm.Funnel.Gated
	case "fired":
		return vm.Funnel.Fired
	case "error":
		return vm.Funnel.Error
	}
	return 0
}

// FooterCount is the footer's "of {count}" number: the active outcome's
// last-24h count, or the whole funnel when no outcome is filtered.
func (vm eventsVM) FooterCount() int {
	if vm.Outcome != "" {
		return vm.FunnelCount(vm.Outcome)
	}
	return vm.Funnel.Total()
}

// SourceHref is the href a source segment carries: the segment's own source
// with the active outcome preserved (params compose), "all" (name == "") the
// reset that drops the source param.
func (vm eventsVM) SourceHref(name string) string {
	var q []string
	if name != "" {
		q = append(q, "source="+name)
	}
	if vm.Outcome != "" {
		q = append(q, "outcome="+vm.Outcome)
	}
	return eventsHref(vm.Page.Base, q)
}

// OutcomeHref is the href an outcome chip carries. The active chip's own href
// is the reset (the outcome param removed, the source kept) — clicking the
// active chip is the clear — while any other chip composes its outcome with
// the active source.
func (vm eventsVM) OutcomeHref(o string) string {
	var q []string
	if vm.Source != "" {
		q = append(q, "source="+vm.Source)
	}
	if o != "" && o != vm.Outcome {
		q = append(q, "outcome="+o)
	}
	return eventsHref(vm.Page.Base, q)
}

func eventsHref(base string, q []string) string {
	path := "/events"
	if len(q) == 0 {
		return urlWithBase(base, path)
	}
	return urlWithBase(base, path) + "?" + strings.Join(q, "&")
}

// commaCount renders a count with thousands separators ("1,204") for the
// count cell's full twin.
func commaCount(n int) string {
	if n < 1000 {
		return itoa(int64(n))
	}
	return commaCount(n/1000) + "," + countPad3(n%1000)
}

func countPad3(n int) string {
	s := itoa(int64(n))
	if n < 100 {
		s = "0" + s
	}
	if n < 10 {
		s = "0" + s
	}
	return s
}

// compactCount renders a count's compact twin ("1.2k", "12.4k") for the
// count cell's k-form, which the phone stylesheet shows from four digits and
// desktop from five; the full twin carries the separator form.
func compactCount(n int) string {
	if n < 1000 {
		return itoa(int64(n))
	}
	tenths := (n*10 + 500) / 1000
	return itoa(int64(tenths/10)) + "." + itoa(int64(tenths%10)) + "k"
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
// copy (SPEC §1a): a silence reads "swallowed · silence {scope} · lifts {t}",
// a cooldown reads "cooldown {d} left", a duplicate reads
// "duplicate of #{id}". Anything else keeps its stored sentence.
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
		rest := reason[len("cooldown "):]
		if i := strings.Index(rest, ","); i > 0 {
			return "cooldown " + rest[:i] + " left"
		}
		return reason
	}
	if strings.HasPrefix(reason, "duplicate of event ") {
		rest := reason[len("duplicate of event "):]
		if i := strings.Index(rest, " "); i > 0 {
			return "duplicate of #" + rest[:i]
		}
		return reason
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

// lagLabel renders the RPO lag the way the card's figure reads it: "42m" on a
// whole minute, "2m 10s" with seconds, "1h 05m" past the hour — never a
// trailing zero unit, so a fresh replica reads "2m 10s" and a stale one "42m"
// (SPEC §3.1).
func lagLabel(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		if int(d.Seconds())%60 == 0 {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		if int(d.Minutes())%60 == 0 {
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// sidecarFact renders the sidecar fact from the presence probe: the versioned
// "litestream 0.3.13 present · runs beside the daemon" when the binary answers,
// "litestream present" when only the binary is found, "litestream binary not on
// PATH" when absent. unsure switches to the "is it running? exocomp cannot
// tell" form the STALE/BEHIND cards use, where the binary exists but the lag
// says it is not keeping up (copy.txt "sidecar unsure").
func sidecarFact(o litestreamObserved, unsure bool) string {
	if o.binPath == "" {
		return "litestream binary not on PATH"
	}
	if unsure {
		return "present · is it running? exocomp cannot tell"
	}
	ver := strings.TrimPrefix(o.version, "litestream ")
	if ver == "" {
		return "litestream present"
	}
	return "litestream " + ver + " present · runs beside the daemon"
}

// scrubReplica renders the stored target for the replica fact with any embedded
// userinfo removed (backup.ScrubURIUserinfo) — a target may carry a password in
// its authority, and a fact must never echo it.
func scrubReplica(raw string) string {
	return backup.ScrubURIUserinfo(raw)
}

// deriveLitestream is the six-word-state derivation (SPEC §3.1), a pure
// function of what the daemon observes. Order is load-bearing: a missing target
// is a gap (dim NO REPLICA), an unparseable target a misconfiguration (wait
// UNPARSEABLE), a set target with no engine a fault (fail NO SIDECAR), a remote
// replica's unreadable lag a fact (dim REMOTE), and only then does the frame's
// age pick REPLICATING (ok <10m) / STALE (wait) / BEHIND (fail ≥1h). A local
// leg with no readable frame is the STALE card's "is it running? exocomp cannot
// tell" — the design draws no seventh word for it, so the wait state carries
// the honest facts instead.
func deriveLitestream(o litestreamObserved) litestreamVM {
	v := litestreamVM{}
	switch {
	case o.raw == "":
		v.State, v.StateClass = "NO REPLICA", "dim"
		v.Figure, v.FigureNote = "—", "no target · the store lives on this disk only"
		v.Sidecar = sidecarFact(o, false)
		v.SetOne = "exocomp litestream target ‹uri›"
		v.Sentence = "Backups still recover the system; this leg only shortens the loss window from a day to minutes."
		v.ConfigErr = "no replica target configured — run `exocomp litestream target <uri>` first"

	case o.parseErr != nil:
		v.State, v.StateClass = "UNPARSEABLE", "wait"
		v.Figure, v.FigureNote = "—", "target unparseable · fix the target"
		v.Replica = scrubReplica(o.raw)
		v.SetOne = "exocomp litestream target ‹uri›"
		v.Sentence = "The target as stored does not parse; fix it with the CLI."
		v.ConfigErr = "fix the target — exocomp litestream target ‹uri›"

	case o.binPath == "":
		v.State, v.StateClass = "NO SIDECAR", "fail"
		v.Figure, v.FigureNote = "—", "target set · litestream binary not on PATH · nothing replicates"
		v.Replica = scrubReplica(o.raw)
		v.ConfigCmd = "exocomp litestream config"
		v.Fix = "install litestream on " + o.host + ", then doctor"
		v.Sentence = "The RPO chip used to say 'no replica configured' for this too. A configured leg with no engine is a failure, not a gap."

	case o.remote:
		v.State, v.StateClass = "REMOTE", "dim"
		v.Figure, v.FigureNote = "unknown", "lag · only litestream can read a remote replica"
		v.Replica = scrubReplica(o.raw)
		if o.ack {
			v.Encryption = "none · unencrypted remote acknowledged"
		} else {
			v.Encryption = "none · not acknowledged"
		}
		v.Sidecar = sidecarFact(o, false)
		v.Sentence = "Configured and not observable — a fact, not a fault. Lag reads from the host: litestream replicas"

	case !o.freshOK || o.freshErr != nil:
		v.State, v.StateClass = "STALE", "wait"
		v.Figure, v.FigureNote = "—", "no frames replicated yet"
		v.Replica = scrubReplica(o.raw)
		v.Sidecar = "present · is it running? exocomp cannot tell"
		v.Sentence = "Check the sidecar on " + o.host + ": systemctl status litestream"

	default:
		v.Replica = scrubReplica(o.raw)
		if o.frameNo > 0 {
			v.Checkpoint = fmt.Sprintf("frame %d · %s", o.frameNo, o.mtime.UTC().Format("15:04:05"))
		}
		switch rpoFreshness(o.age) {
		case "fail":
			v.State, v.StateClass = "BEHIND", "fail"
			v.Figure, v.FigureNote = lagLabel(o.age), "behind · ok < 10m · wait < 1h · fail"
			v.Sidecar = "present · is it running? exocomp cannot tell"
			v.Sentence = "Check the sidecar on " + o.host + ": systemctl status litestream"
		case "wait":
			v.State, v.StateClass = "STALE", "wait"
			v.Figure, v.FigureNote = lagLabel(o.age), "behind · ok < 10m · wait < 1h · fail"
			v.Sidecar = "present · is it running? exocomp cannot tell"
			v.Sentence = "Check the sidecar on " + o.host + ": systemctl status litestream"
		default:
			v.State, v.StateClass = "REPLICATING", "ok"
			v.Figure, v.FigureNote = lagLabel(o.age), "behind · RPO"
			v.Sidecar = sidecarFact(o, false)
			v.Sentence = "exocomp writes the config and reads the lag; it does not run the sidecar, so there is no pause here. Fast restore: exocomp restore --from-litestream"
		}
	}
	return v
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

// platformLabel is the identity strip's "{os}/{arch}" — the same shape the
// CLI doctor reports, so the page and the CLI never disagree about the build.
func platformLabel() string { return runtime.GOOS + "/" + runtime.GOARCH }

// backupsPowerWord derives the self-care powerline's backups segment from the
// freshness rows the Backups card draws (never a second read): "backups ok"
// when every ENABLED target's newest run succeeded, "backups none" when no
// enabled target exists or one has never run (a gap, wait — never a fail), and
// "backups {n} failing" when runs have failed — wait for one, fail for two or
// more, the card chip's own rule. A disabled target contributes nothing.
func backupsPowerWord(rows []backupRow) (string, string) {
	nFailed := 0
	nNever := 0
	nEnabled := 0
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		nEnabled++
		switch r.State {
		case "failed":
			nFailed++
		case "never":
			nNever++
		}
	}
	if nEnabled == 0 || nNever > 0 {
		return "backups none", "wait"
	}
	if nFailed == 0 {
		return "backups ok", "ok"
	}
	if nFailed >= 2 {
		return fmt.Sprintf("backups %d failing", nFailed), "fail"
	}
	return "backups 1 failing", "wait"
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
