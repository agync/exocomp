package wire

import (
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Kinds name each payload so an agent dispatches without guessing.
const (
	KindJobList       = "job.list"
	KindJob           = "job.get"
	KindJobEvents     = "job.events"
	KindWorkspaceList = "workspace.list"
	KindSourceList    = "source.list"
	KindApprovalList  = "approval.list"
	KindScheduleList  = "schedule.list"
	KindRuleList      = "rule.list"
	KindSilenceList   = "silence.list"
	KindEventList     = "event.list"
	KindSecretList    = "secret.list"
	KindBackupList    = "backup.list"
	KindTokenList     = "token.list"
	KindPolicyList    = "policy.list"
	KindCommands      = "commands"
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
type JobView struct {
	ID             int64   `json:"id"`
	Runbook        string  `json:"runbook,omitempty"`
	Kind           string  `json:"kind,omitempty"`
	Trigger        string  `json:"trigger,omitempty"`
	State          string  `json:"state"`
	ExitCode       *int    `json:"exit_code,omitempty"`
	Partial        bool    `json:"partial,omitempty"`
	ConcurrencyKey string  `json:"concurrency_key,omitempty"`
	QueuedAt       string  `json:"queued_at,omitempty"`
	StartedAt      *string `json:"started_at,omitempty"`
	FinishedAt     *string `json:"finished_at,omitempty"`
}

// NewJobView projects one job.
func NewJobView(j store.Job) JobView {
	return JobView{
		ID:             j.ID,
		Runbook:        j.Runbook,
		Kind:           j.Kind,
		Trigger:        j.Trigger,
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
// omitted to stay low-token; msg is the redacted line the sink wrote.
type JobEventView struct {
	Seq   int64  `json:"seq"`
	Ts    string `json:"ts"`
	Level string `json:"level,omitempty"`
	Msg   string `json:"msg,omitempty"`
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
		})
	}
	return out
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
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// NewSourceList projects sources in store order.
func NewSourceList(sources []store.Source) []SourceView {
	out := make([]SourceView, 0, len(sources))
	for _, s := range sources {
		out = append(out, SourceView{
			ID: s.ID, Name: s.Name, Kind: s.Kind, URI: s.URI, Ref: s.Ref,
			LastSyncSHA: s.LastSyncSHA, Signed: s.Signed, Enabled: s.Enabled,
			CreatedAt: rfc3339(s.CreatedAt),
		})
	}
	return out
}

// ApprovalView projects an approval. The token hash — the capability material —
// never travels.
type ApprovalView struct {
	ID          int64   `json:"id"`
	JobID       int64   `json:"job_id"`
	State       string  `json:"state"`
	Workspace   string  `json:"workspace,omitempty"`
	PlanSerial  int64   `json:"plan_serial,omitempty"`
	RequestedAt string  `json:"requested_at,omitempty"`
	ExpiresAt   string  `json:"expires_at,omitempty"`
	DecidedAt   *string `json:"decided_at,omitempty"`
	DecidedVia  string  `json:"decided_via,omitempty"`
	Reason      string  `json:"reason,omitempty"`
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
// not a value; the action argv is omitted.
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
}

// NewRuleList projects rules in store order.
func NewRuleList(rows []store.Rule) []RuleView {
	out := make([]RuleView, 0, len(rows))
	for _, r := range rows {
		out = append(out, RuleView{
			ID: r.ID, Name: r.Name, SourceFilter: r.SourceFilter, CELExpr: r.CELExpr,
			ActionKind: r.ActionKind, CooldownS: r.CooldownS, DailyCap: r.DailyCap,
			RequireApproval: r.RequireApproval, Enabled: r.Enabled,
		})
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
// the trace is the `event explain` surface, not the list.
type EventView struct {
	ID          int64  `json:"id"`
	Source      string `json:"source"`
	ReceivedAt  string `json:"received_at"`
	DedupKey    string `json:"dedup_key,omitempty"`
	MatchedRule string `json:"matched_rule,omitempty"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	JobID       *int64 `json:"job_id,omitempty"`
}

// NewEventList projects events newest-first, as the store returns them.
func NewEventList(rows []store.Event) []EventView {
	out := make([]EventView, 0, len(rows))
	for _, e := range rows {
		out = append(out, EventView{
			ID: e.ID, Source: e.Source, ReceivedAt: rfc3339(e.ReceivedAt), DedupKey: e.DedupKey,
			MatchedRule: e.MatchedRule, Outcome: string(e.Outcome), Reason: e.Reason, JobID: e.JobID,
		})
	}
	return out
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
