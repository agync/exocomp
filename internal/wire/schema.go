package wire

import (
	"encoding/json"
	"net/netip"
	"slices"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// Kinds name each payload so an agent dispatches without guessing.
const (
	KindJobList       = "job.list"
	KindJob           = "job.get"
	KindJobEvents     = "job.events"
	KindWorkspaceList = "workspace.list"
	KindSourceList    = "source.list"
	KindApproval      = "approval.get"
	KindApprovalList  = "approval.list"
	KindScheduleList  = "schedule.list"
	KindNetgroupList  = "netgroup.list"
	KindCheckList     = "check.list"
	KindRuleList      = "rule.list"
	KindSilenceList   = "silence.list"
	KindEvent         = "event.get"
	KindEventList     = "event.list"
	KindEventSummary  = "event.summary"
	KindSecretList    = "secret.list"
	KindBackupList    = "backup.list"
	KindTokenList     = "token.list"
	KindPolicyList    = "policy.list"
	KindCommands      = "commands"
	KindDoctor        = "doctor"
)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func rfc3339p(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// JobView projects a job row: identifiers, state, exit code, and the lifecycle
// timestamps. Args (which can carry operator-supplied values), dedup and
// idempotency keys, and the owner pid are deliberately omitted.
//
// Plan is populated separately from the row — it comes from the job's own
// events, not a store.Job column — so NewJobView alone always leaves it nil.
// A caller that has the job's events attaches it with NewPlanOutcomeView (see
// internal/web/api.go's apiJobGet, the primary seam a remote `workspace plan`
// reads to reproduce the local exit-2-on-changes decision without RawJSON).
type JobView struct {
	ID      int64  `json:"id"`
	Runbook string `json:"runbook,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Trigger string `json:"trigger,omitempty"`
	// Source is the registered source this job ran from (job.Args.Source),
	// unlike the rest of Args it is a display fact, not operator-supplied
	// material, so it is projected here rather than omitted with the row.
	Source         string           `json:"source,omitempty"`
	State          string           `json:"state"`
	ExitCode       *int             `json:"exit_code,omitempty"`
	Partial        bool             `json:"partial,omitempty"`
	ConcurrencyKey string           `json:"concurrency_key,omitempty"`
	QueuedAt       string           `json:"queued_at,omitempty"`
	StartedAt      *string          `json:"started_at,omitempty"`
	FinishedAt     *string          `json:"finished_at,omitempty"`
	Plan           *PlanOutcomeView `json:"plan,omitempty"`
}

// NewJobView projects one job.
func NewJobView(j store.Job) JobView {
	return JobView{
		ID:             j.ID,
		Runbook:        j.Runbook,
		Kind:           j.Kind,
		Trigger:        j.Trigger,
		Source:         j.Args.Source,
		State:          string(j.State),
		ExitCode:       j.ExitCode,
		Partial:        j.Partial,
		ConcurrencyKey: j.ConcurrencyKey,
		QueuedAt:       rfc3339(j.QueuedAt),
		StartedAt:      rfc3339p(j.StartedAt),
		FinishedAt:     rfc3339p(j.FinishedAt),
	}
}

// NewJobList projects a job list, preserving the store's oldest-first order.
func NewJobList(jobs []store.Job) []JobView {
	out := make([]JobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, NewJobView(j))
	}
	return out
}

// JobEventView projects one append-only, already-redacted job event. RawJSON is
// omitted to stay low-token; msg is the redacted line the sink wrote. Plan is
// the one deliberate exception: a plan/check outcome event's raw_json, reduced
// to the same values-free PlanOutcomeView JobView.Plan carries, so a remote
// reader tailing events sees the machine-parseable outcome without RawJSON.
// It is nil for every event but the one carrying that payload.
type JobEventView struct {
	Seq   int64            `json:"seq"`
	Ts    string           `json:"ts"`
	Level string           `json:"level,omitempty"`
	Msg   string           `json:"msg,omitempty"`
	Plan  *PlanOutcomeView `json:"plan,omitempty"`
}

// NewJobEventList projects a job's events in sequence order.
func NewJobEventList(events []store.JobEvent) []JobEventView {
	out := make([]JobEventView, 0, len(events))
	for _, e := range events {
		out = append(out, JobEventView{
			Seq:   e.Seq,
			Ts:    rfc3339(e.Ts),
			Level: e.Level,
			Msg:   e.Msg,
			Plan:  planOutcomeFromRawJSON(e.RawJSON),
		})
	}
	return out
}

// PlanOutcomeView is the values-free summary of a plan (tofu) or check
// (ansible) job's outcome: whether the run found changes, and — when the
// producing event carried resource/host detail — how many resources would be
// added, changed, or destroyed (ansible has no add/destroy distinction, so a
// host with drift counts as one Change) and their addresses. It mirrors
// TofuPlanOutcome/AnsibleCheckOutcome's own values-free discipline exactly:
// no field here could ever hold an attribute value, because none of the
// structs it is built from carries one.
type PlanOutcomeView struct {
	Changes   bool     `json:"changes"`
	Add       int      `json:"add,omitempty"`
	Change    int      `json:"change,omitempty"`
	Destroy   int      `json:"destroy,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// NewPlanOutcomeView scans a job's events, oldest first, for the one event
// carrying a plan or check outcome and projects it. It is the wire-side twin
// of cmd/exocomp/workspace.go's planChanges: the same "read the terminal plan
// event back out of the job's own events" lookup, so a remote client can
// reach the same exit-2-on-changes verdict the local CLI reaches from RawJSON
// directly. nil when no event in the job carries either payload (including
// every non-plan, non-check job).
func NewPlanOutcomeView(events []store.JobEvent) *PlanOutcomeView {
	for _, e := range events {
		if v := planOutcomeFromRawJSON(e.RawJSON); v != nil {
			return v
		}
	}
	return nil
}

// planOutcomeFromRawJSON tries one event's raw_json against both outcome
// shapes a plan-family job can carry. Trying tofu first is not a behavioural
// choice — the two envelope keys ("tofu" / "ansible_check") never both match
// the same document, so the order cannot change which one is found, only
// which unmarshal runs first.
func planOutcomeFromRawJSON(rawJSON string) *PlanOutcomeView {
	if outcome, ok := executor.TofuPlanOutcomeFor(rawJSON); ok {
		return tofuPlanOutcomeView(outcome)
	}
	if outcome, ok := executor.AnsibleCheckChanges(rawJSON); ok {
		return ansibleCheckOutcomeView(outcome)
	}
	return nil
}

// tofuPlanOutcomeView reduces a TofuPlanOutcome to its values-free wire
// summary. A replace (actions ["delete","create"], either order) counts in
// both Add and Destroy, matching tofu's own "N to add, M to change, K to
// destroy" convention for a resource that is torn down and rebuilt.
func tofuPlanOutcomeView(o executor.TofuPlanOutcome) *PlanOutcomeView {
	v := &PlanOutcomeView{Changes: o.Changes}
	deltas := make([]executor.ResourceDelta, 0, len(o.Drift)+len(o.Pending))
	deltas = append(deltas, o.Drift...)
	deltas = append(deltas, o.Pending...)
	for _, d := range deltas {
		v.Addresses = append(v.Addresses, d.Address)
		add := slices.Contains(d.Actions, "create")
		destroy := slices.Contains(d.Actions, "delete")
		switch {
		case add && destroy:
			v.Add++
			v.Destroy++
		case add:
			v.Add++
		case destroy:
			v.Destroy++
		default:
			v.Change++
		}
	}
	return v
}

// ansibleCheckOutcomeView reduces an AnsibleCheckOutcome to its values-free
// wire summary. Ansible's check mode has no add/destroy distinction — a task
// either would change a host or would not — so every drifted host counts as
// one Change, named by host rather than resource address.
func ansibleCheckOutcomeView(o executor.AnsibleCheckOutcome) *PlanOutcomeView {
	v := &PlanOutcomeView{Changes: o.Changes}
	for _, d := range o.Drift {
		v.Addresses = append(v.Addresses, d.Host)
		v.Change++
	}
	return v
}

// WorkspaceView projects a workspace: name, id, the newest stored serial, and
// the tofu directory it runs from.
type WorkspaceView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Serial  int64  `json:"serial"`
	TofuDir string `json:"tofu_dir,omitempty"`
}

// NewWorkspaceList projects workspaces in store order.
func NewWorkspaceList(ws []store.Workspace) []WorkspaceView {
	out := make([]WorkspaceView, 0, len(ws))
	for _, w := range ws {
		out = append(out, WorkspaceView{ID: w.ID, Name: w.Name, Serial: w.Serial, TofuDir: w.TofuDir})
	}
	return out
}

// SourceView projects a source: identity, pin, and flags. The deploy-key
// secret name is a reference, not a value; allowed-signers content is omitted.
type SourceView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URI         string `json:"uri,omitempty"`
	Ref         string `json:"ref,omitempty"`
	LastSyncSHA string `json:"last_sync_sha,omitempty"`
	Signed      bool   `json:"signed,omitempty"`
	// Writable reports whether a path source's tree is granted a read+write
	// working directory. Always false for a git source.
	Writable  bool   `json:"writable,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
}

// NewSourceList projects sources in store order.
func NewSourceList(sources []store.Source) []SourceView {
	out := make([]SourceView, 0, len(sources))
	for _, s := range sources {
		out = append(out, SourceView{
			ID: s.ID, Name: s.Name, Kind: s.Kind, URI: s.URI, Ref: s.Ref,
			LastSyncSHA: s.LastSyncSHA, Signed: s.Signed, Writable: s.Writable, Enabled: s.Enabled,
			CreatedAt: rfc3339(s.CreatedAt),
		})
	}
	return out
}

// ApprovalView projects an approval. The token hash — the capability material —
// never travels. Stale and Provenance are populated only on the single-object
// `.get` view (NewApprovalView leaves them nil): Stale is present only when the
// workspace moved under the saved plan, and Provenance's alert/rule/matched_at
// are present only for an approval a reactive rule raised — both carry
// identifiers and states only, never a plan attribute value.
type ApprovalView struct {
	ID          int64                   `json:"id"`
	JobID       int64                   `json:"job_id"`
	State       string                  `json:"state"`
	Workspace   string                  `json:"workspace,omitempty"`
	PlanSerial  int64                   `json:"plan_serial,omitempty"`
	RequestedAt string                  `json:"requested_at,omitempty"`
	ExpiresAt   string                  `json:"expires_at,omitempty"`
	DecidedAt   *string                 `json:"decided_at,omitempty"`
	DecidedVia  string                  `json:"decided_via,omitempty"`
	Reason      string                  `json:"reason,omitempty"`
	Stale       *ApprovalStaleView      `json:"stale,omitempty"`
	Provenance  *ApprovalProvenanceView `json:"provenance,omitempty"`
}

// The approval provenance origins: an approval a reactive rule raised from an
// alert, one an operator asked for by hand (CLI/API), or one a schedule fired.
const (
	OriginAlert    = "alert"
	OriginManual   = "manual"
	OriginSchedule = "schedule"
)

// ApprovalStaleView is the structured stale-plan refusal: the plan's serial,
// the serial the workspace stands at now, and — when the control plane owns the
// state version that moved it — which job did so, under what trigger, and when.
// It carries no plan content, only the movement facts.
type ApprovalStaleView struct {
	PlanSerial    int64   `json:"plan_serial"`
	CurrentSerial int64   `json:"current_serial"`
	MovedByJob    *int64  `json:"moved_by_job,omitempty"`
	MovedVia      string  `json:"moved_via,omitempty"`
	MovedAt       *string `json:"moved_at,omitempty"`
}

// NewApprovalStaleView projects a PlanMovement. It is nil-safe: a non-stale
// movement is not a view, so callers pass one only when staleness was reported.
func NewApprovalStaleView(m store.PlanMovement) *ApprovalStaleView {
	v := &ApprovalStaleView{
		PlanSerial:    m.PlanSerial,
		CurrentSerial: m.CurrentSerial,
		MovedByJob:    m.MovedByJob,
		MovedVia:      m.MovedVia,
	}
	if !m.MovedAt.IsZero() {
		at := rfc3339(m.MovedAt)
		v.MovedAt = &at
	}
	return v
}

// ApprovalProvenanceView answers screen 02's "why you": the origin an approval
// arose from and, for an alert-raised one, the alert, the rule that matched it,
// and when. alert/rule/matched_at are absent for a manual or scheduled origin.
type ApprovalProvenanceView struct {
	Origin    string  `json:"origin"`
	Alert     string  `json:"alert,omitempty"`
	Rule      string  `json:"rule,omitempty"`
	MatchedAt *string `json:"matched_at,omitempty"`
}

// NewApprovalProvenanceView projects one provenance. matchedAt is a pointer so
// a manual/scheduled origin (no matching event) renders no timestamp.
func NewApprovalProvenanceView(origin, alert, rule string, matchedAt *time.Time) *ApprovalProvenanceView {
	return &ApprovalProvenanceView{
		Origin:    origin,
		Alert:     alert,
		Rule:      rule,
		MatchedAt: rfc3339p(matchedAt),
	}
}

// NewApprovalView projects one approval. It mirrors NewApprovalList's element
// mapping exactly, so a single-object `.get` and a one-element `.list` project
// a row identically.
func NewApprovalView(a store.Approval) ApprovalView {
	return ApprovalView{
		ID: a.ID, JobID: a.JobID, State: string(a.State), Workspace: a.Workspace,
		PlanSerial: a.PlanSerial, RequestedAt: rfc3339(a.RequestedAt),
		ExpiresAt: rfc3339(a.ExpiresAt), DecidedAt: rfc3339p(a.DecidedAt),
		DecidedVia: a.DecidedVia, Reason: a.Reason,
	}
}

// NewApprovalList projects approvals in store order.
func NewApprovalList(rows []store.Approval) []ApprovalView {
	out := make([]ApprovalView, 0, len(rows))
	for _, a := range rows {
		out = append(out, ApprovalView{
			ID: a.ID, JobID: a.JobID, State: string(a.State), Workspace: a.Workspace,
			PlanSerial: a.PlanSerial, RequestedAt: rfc3339(a.RequestedAt),
			ExpiresAt: rfc3339(a.ExpiresAt), DecidedAt: rfc3339p(a.DecidedAt),
			DecidedVia: a.DecidedVia, Reason: a.Reason,
		})
	}
	return out
}

// ScheduleView projects a schedule. Its argv/target live in Args and are
// omitted to stay identifier-only.
type ScheduleView struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Cron           string  `json:"cron"`
	Kind           string  `json:"kind"`
	ConcurrencyKey string  `json:"concurrency_key,omitempty"`
	JitterS        int     `json:"jitter_s,omitempty"`
	Enabled        bool    `json:"enabled"`
	LastJobID      *int64  `json:"last_job_id,omitempty"`
	NextAt         *string `json:"next_at,omitempty"`
}

// NewScheduleList projects schedules in store order.
func NewScheduleList(rows []store.Schedule) []ScheduleView {
	out := make([]ScheduleView, 0, len(rows))
	for _, s := range rows {
		out = append(out, ScheduleView{
			ID: s.ID, Name: s.Name, Cron: s.CronExpr, Kind: s.Kind,
			ConcurrencyKey: s.ConcurrencyKey, JitterS: s.JitterS, Enabled: s.Enabled,
			LastJobID: s.LastJobID, NextAt: rfc3339p(s.NextAt),
		})
	}
	return out
}

// RuleView projects a rule and its guardrails. The CEL expression is config,
// not a value; the action argv is omitted. The playbook-action fields are
// present only for an ActionKind of playbook.
type RuleView struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	SourceFilter    string `json:"source_filter,omitempty"`
	CELExpr         string `json:"cel_expr,omitempty"`
	ActionKind      string `json:"action_kind"`
	CooldownS       int    `json:"cooldown_s,omitempty"`
	DailyCap        int    `json:"daily_cap,omitempty"`
	RequireApproval bool   `json:"require_approval,omitempty"`
	Enabled         bool   `json:"enabled"`
	// Source names the playbook action's registered source (json "source" to
	// match RuleAction; a rule's event-source filter is "source_filter" above).
	Source   string   `json:"source,omitempty"`
	Playbook string   `json:"playbook,omitempty"`
	Limit    string   `json:"limit,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Check    bool     `json:"check,omitempty"`
	Diff     bool     `json:"diff,omitempty"`
	// Timeout is the action's job kill-switch, as the Go duration string the
	// CLI accepts, so a consumer can audit which rules carry a bound.
	Timeout string `json:"timeout,omitempty"`
}

// NewRuleList projects rules in store order.
func NewRuleList(rows []store.Rule) []RuleView {
	out := make([]RuleView, 0, len(rows))
	for _, r := range rows {
		v := RuleView{
			ID: r.ID, Name: r.Name, SourceFilter: r.SourceFilter, CELExpr: r.CELExpr,
			ActionKind: r.ActionKind, CooldownS: r.CooldownS, DailyCap: r.DailyCap,
			RequireApproval: r.RequireApproval, Enabled: r.Enabled,
			Source: r.Action.Source, Playbook: r.Action.Playbook, Limit: r.Action.Limit,
			Tags: r.Action.Tags, Check: r.Action.Check, Diff: r.Action.Diff,
		}
		if r.Action.Timeout != 0 {
			v.Timeout = time.Duration(r.Action.Timeout).String()
		}
		out = append(out, v)
	}
	return out
}

// SilenceView projects a silence.
type SilenceView struct {
	ID         int64  `json:"id"`
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value,omitempty"`
	Reason     string `json:"reason,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	ExpiresAt  string `json:"expires_at"`
	CreatedVia string `json:"created_via,omitempty"`
}

// NewSilenceList projects silences in store order.
func NewSilenceList(rows []store.Silence) []SilenceView {
	out := make([]SilenceView, 0, len(rows))
	for _, s := range rows {
		out = append(out, SilenceView{
			ID: s.ID, ScopeKind: s.ScopeKind, ScopeValue: s.ScopeValue, Reason: s.Reason,
			CreatedAt: rfc3339(s.CreatedAt), ExpiresAt: rfc3339(s.ExpiresAt), CreatedVia: s.CreatedVia,
		})
	}
	return out
}

// EventView projects a received event and its outcome. The normalized/raw
// payloads and the per-condition trace (which can carry values) are omitted;
// the trace is the `event explain` surface, not the list. Name, Status, and
// Netgroup are the values-free projections of the normalized body the web's
// event surfaces already render (see EventName), so a phone reads a check row
// without parsing stored JSON.
type EventView struct {
	ID          int64  `json:"id"`
	Source      string `json:"source"`
	ReceivedAt  string `json:"received_at"`
	DedupKey    string `json:"dedup_key,omitempty"`
	MatchedRule string `json:"matched_rule,omitempty"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	JobID       *int64 `json:"job_id,omitempty"`
	// Name is the human-facing event name: the normalized body's name, the
	// "check · target" pairing for a check transition, or the source.
	Name string `json:"name,omitempty"`
	// Status is a check transition's direction (firing/resolved) as the sweep
	// observed it; absent for alert events.
	Status string `json:"status,omitempty"`
	// Netgroup is a check event's netgroup classification from the normalized
	// body's labels.netgroup — the group whose prefix covered the target, or
	// "away" when none did, the same value CheckTargetView carries — absent for
	// alert events and http URL targets.
	Netgroup string `json:"netgroup,omitempty"`
}

// NewEventView projects one received event. The normalized/raw payloads and the
// per-condition trace (which can carry values) are omitted, as they are from the
// list; the additive name/status/netgroup fields are the values-free projection
// of the normalized body described on EventView.
func NewEventView(e store.Event) EventView {
	name, status, netgroup := eventViewExtras(e)
	return EventView{
		ID: e.ID, Source: e.Source, ReceivedAt: rfc3339(e.ReceivedAt), DedupKey: e.DedupKey,
		MatchedRule: e.MatchedRule, Outcome: string(e.Outcome), Reason: e.Reason, JobID: e.JobID,
		Name: name, Status: status, Netgroup: netgroup,
	}
}

// NewEventList projects events newest-first, as the store returns them.
func NewEventList(rows []store.Event) []EventView {
	out := make([]EventView, 0, len(rows))
	for _, e := range rows {
		out = append(out, NewEventView(e))
	}
	return out
}

// eventBody is the subset of a normalized event body the projections read: the
// name an alert carries, the check/target pairing a check transition uses
// instead, the transition direction, and the label set a location verdict lives
// in. It is parsed error-tolerantly, like planOutcomeFromRawJSON: an
// unparseable body yields zero values, never an error.
type eventBody struct {
	Name   string            `json:"name"`
	Check  string            `json:"check"`
	Target string            `json:"target"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
}

func parseEventBody(e store.Event) (eventBody, bool) {
	var b eventBody
	if err := json.Unmarshal([]byte(e.Normalized), &b); err != nil {
		return eventBody{}, false
	}
	return b, true
}

// eventViewExtras projects the values-free name/status/netgroup of one event's
// normalized body, sharing EventName with the web so the two surfaces render
// the same row facts.
func eventViewExtras(e store.Event) (name, status, netgroup string) {
	name = EventName(e)
	if e.Source != reactive.SourceCheck {
		return name, "", ""
	}
	b, ok := parseEventBody(e)
	if !ok {
		return name, "", ""
	}
	return name, b.Status, b.Labels["netgroup"]
}

// EventName pulls a human-facing alert name out of a normalized event body,
// falling back to the source when the body carries none. A check transition
// body carries {check,target} instead of a name, so its alert reads as the
// check and target the sweep observed — the Source column already says "check".
// It is the one derivation the web and the API both render through.
func EventName(e store.Event) string {
	b, ok := parseEventBody(e)
	if !ok {
		return e.Source
	}
	if b.Name != "" {
		return b.Name
	}
	if e.Source == reactive.SourceCheck && b.Check != "" {
		if b.Target != "" {
			return b.Check + " · " + b.Target
		}
		return b.Check
	}
	return e.Source
}

// EventSummaryView is the event funnel: every stored event, tallied by the
// same store.EventOutcome names EventView.Outcome and `event list --outcome`
// use — ignored/matched/suppressed/gated/fired/error — never a separate
// taxonomy. Received is the total, all-time (the underlying aggregation is
// unwindowed, like the rest of the events reads), so a phone screen renders
// the whole funnel from one object instead of paging rows to tally it itself.
type EventSummaryView struct {
	Received   int `json:"received"`
	Ignored    int `json:"ignored"`
	Matched    int `json:"matched"`
	Suppressed int `json:"suppressed"`
	Gated      int `json:"gated"`
	Fired      int `json:"fired"`
	Error      int `json:"error"`
}

// NewEventSummaryView projects the store's per-outcome event tally.
func NewEventSummaryView(c store.EventOutcomeCounts) EventSummaryView {
	return EventSummaryView{
		Received: c.Total(), Ignored: c.Ignored, Matched: c.Matched,
		Suppressed: c.Suppressed, Gated: c.Gated, Fired: c.Fired, Error: c.Error,
	}
}

// SecretView is metadata only. Nonce, ciphertext, and AAD are structurally
// absent from this type: there is no field they could occupy.
type SecretView struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Source     string  `json:"source,omitempty"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	CreatedAt  string  `json:"created_at"`
	RotatedAt  *string `json:"rotated_at,omitempty"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
}

// NewSecretList projects secret metadata in store order. It reads only the
// non-secret columns of each row.
func NewSecretList(rows []store.SecretRow) []SecretView {
	out := make([]SecretView, 0, len(rows))
	for _, s := range rows {
		out = append(out, SecretView{
			ID: s.ID, Name: s.Name, Kind: s.Kind, Source: s.Source,
			ExpiresAt: rfc3339p(s.ExpiresAt), CreatedAt: rfc3339(s.CreatedAt),
			RotatedAt: rfc3339p(s.RotatedAt), LastUsedAt: rfc3339p(s.LastUsedAt),
		})
	}
	return out
}

// BackupTargetView projects a backup target.
type BackupTargetView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URI       string `json:"uri,omitempty"`
	Retention string `json:"retention,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// NewBackupTargetList projects backup targets in store order.
func NewBackupTargetList(rows []store.BackupTarget) []BackupTargetView {
	out := make([]BackupTargetView, 0, len(rows))
	for _, t := range rows {
		out = append(out, BackupTargetView{
			ID: t.ID, Name: t.Name, Kind: t.Kind, URI: t.URI,
			Retention: t.Retention, Enabled: t.Enabled,
		})
	}
	return out
}

// TokenView projects an access token as metadata. The argon2id hash never
// travels; the prefix is the public, non-secret token label.
type TokenView struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	Prefix     string  `json:"prefix,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

// NewTokenList projects tokens in store order.
func NewTokenList(rows []store.APIToken) []TokenView {
	out := make([]TokenView, 0, len(rows))
	for _, t := range rows {
		out = append(out, TokenView{
			ID: t.ID, Name: t.Name, Scope: t.Scope, Prefix: t.Prefix,
			CreatedAt: rfc3339(t.CreatedAt), ExpiresAt: rfc3339p(t.ExpiresAt),
			LastUsedAt: rfc3339p(t.LastUsedAt), RevokedAt: rfc3339p(t.RevokedAt),
		})
	}
	return out
}

// PolicyView projects a plan policy.
type PolicyView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
	Expr      string `json:"expr,omitempty"`
	Action    string `json:"action"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
}

// NewPolicyList projects plan policies in store order (by name).
func NewPolicyList(rows []store.PlanPolicy) []PolicyView {
	out := make([]PolicyView, 0, len(rows))
	for _, p := range rows {
		out = append(out, PolicyView{
			ID: p.ID, Name: p.Name, Workspace: p.Workspace, Expr: p.Expr,
			Action: p.Action, Enabled: p.Enabled, CreatedAt: rfc3339(p.CreatedAt),
		})
	}
	return out
}

// NetgroupView projects a netgroup: its name and the CIDRs it names. A prefix
// is config, not a value, so it travels in full.
type NetgroupView struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Prefixes []string `json:"prefixes"`
}

// NewNetgroupList projects netgroups in store order (by name).
func NewNetgroupList(rows []store.Netgroup) []NetgroupView {
	out := make([]NetgroupView, 0, len(rows))
	for _, g := range rows {
		prefixes := make([]string, len(g.Prefixes))
		for i, p := range g.Prefixes {
			prefixes[i] = p.String()
		}
		out = append(out, NetgroupView{ID: g.ID, Name: g.Name, Prefixes: prefixes})
	}
	return out
}

// CheckTargetView projects one probe of a check. Status is the sweep's last
// observation of this target ("" when the sweep has never seen it); Netgroup is
// the classification of the probed address ("" for an http URL target, "away"
// when no registered group covers it); LastChange is when that status last
// changed ("" when the sweep has never seen it). The address is the target's
// location, so it travels in full.
type CheckTargetView struct {
	Name       string `json:"name"`
	Address    string `json:"address,omitempty"`
	URL        string `json:"url,omitempty"`
	Port       int    `json:"port,omitempty"`
	Expect     int    `json:"expect,omitempty"`
	Status     string `json:"status,omitempty"`
	Netgroup   string `json:"netgroup,omitempty"`
	LastChange string `json:"last_change,omitempty"`
}

// CheckView projects a check and its per-target last statuses.
type CheckView struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	IntervalS int64             `json:"interval_s"`
	Enabled   bool              `json:"enabled"`
	Targets   []CheckTargetView `json:"targets"`
}

// NewCheckList projects checks in store order (by name), joining each target's
// last observed status and its recency from the sweep's check_state rows and
// its netgroup classification from the caller's one-pass map. The map is keyed
// by the target address's canonical (unmapped) string; an http URL target has
// no address and no netgroup.
func NewCheckList(rows []store.Check, states map[int64][]store.CheckState, netgroups map[string]string) []CheckView {
	out := make([]CheckView, 0, len(rows))
	for _, c := range rows {
		byName := make(map[string]store.CheckState, len(states[c.ID]))
		for _, s := range states[c.ID] {
			byName[s.TargetName] = s
		}
		targets := make([]CheckTargetView, 0, len(c.Targets))
		for _, t := range c.Targets {
			v := CheckTargetView{
				Name: t.Name, Address: t.Address, URL: t.URL,
				Port: t.Port, Expect: t.Expect,
			}
			if st, ok := byName[t.Name]; ok {
				v.Status = st.Status
				v.LastChange = rfc3339(st.LastChange)
			}
			if t.Address != "" {
				v.Netgroup = netgroups[netip.MustParseAddr(t.Address).Unmap().String()]
			}
			targets = append(targets, v)
		}
		out = append(out, CheckView{
			ID: c.ID, Name: c.Name, Kind: c.Kind,
			IntervalS: c.IntervalS, Enabled: c.Enabled, Targets: targets,
		})
	}
	return out
}
