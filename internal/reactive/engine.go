package reactive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// Defaults for the two windows the engine is configured with.
const (
	DefaultDedupWindow = 5 * time.Minute
	DefaultApprovalTTL = 30 * time.Minute
)

// Gate is the part of the job queue the engine uses. It is an interface so a
// test can watch what reached the queue without running one, and so the engine
// cannot reach past Enqueue into execution.
type Gate interface {
	Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error)
	EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error)
	Release(ctx context.Context, jobID int64) error
	Withdraw(ctx context.Context, jobID int64) error
}

// Redactor scrubs known secret values. A webhook body is attacker-influenced
// text that lands in a durable table, so it goes through the same filter job
// output does (AGENTS.md invariant 4).
type Redactor interface {
	Redact([]byte) []byte
}

// Options configures an Engine.
type Options struct {
	// Store holds every ledger the guardrails read.
	Store *store.Store
	// Queue is the only way an action reaches execution.
	Queue Gate
	// Deriver mints the decision capabilities and the hook tokens.
	Deriver Deriver
	// Notifier carries approvals and cap trips to the operator's phone.
	Notifier *notify.Notifier
	// Playbooks resolves a playbook-action rule's source checkout into the
	// spec the queue takes. Nil is allowed — the web engine and the CLI
	// approval engine construct a nil-builder engine — but a playbook-action
	// rule matching on such an engine records an outcome error and fires
	// nothing.
	Playbooks PlaybookBuilder
	// BaseURL is what a decision link is rooted at, e.g. http://127.0.0.1:8080.
	BaseURL string
	// DedupWindow is how long a repeat of the same condition is a duplicate.
	// Default DefaultDedupWindow.
	DedupWindow time.Duration
	// ApprovalTTL is how long a decision link is good for. Default
	// DefaultApprovalTTL. Expiry is fail-closed: the job is rejected.
	ApprovalTTL time.Duration
	// Redactor resolves the filter that scrubs sealed secret values out of
	// stored bodies. It is a function, resolved once per recorded event, so a
	// secret sealed after the daemon started is scrubbed without a restart.
	Redactor func() Redactor
	// Clock overrides time.Now, for tests.
	Clock func() time.Time
	// Logger receives failures that are nobody's job. Default slog.Default().
	Logger *slog.Logger
}

// Engine decides what a received event is allowed to do.
type Engine struct {
	store       *store.Store
	queue       Gate
	deriver     Deriver
	notifier    *notify.Notifier
	playbooks   PlaybookBuilder
	dedupWindow time.Duration
	approvalTTL time.Duration
	redactor    func() Redactor
	clock       func() time.Time
	log         *slog.Logger

	// republished remembers which approvals have had their 50%-TTL reminder.
	// It is process state on purpose: the reminder is a nudge, and a daemon
	// restart sending one more is better than a column in the store. Each
	// SweepApprovals call prunes entries for approvals no longer pending, so
	// this stays bounded by the current pending count rather than growing
	// across the daemon's lifetime.
	republished map[int64]bool

	// capMu guards capPaged: the last time a rule's daily cap paged, so a flood
	// past the cap alerts loudly once per rolling window instead of once per
	// event. Process state on purpose, like republished — a restart re-pages
	// once, which is louder than silent, not a ledger that has to be swept.
	capMu    sync.Mutex
	capPaged map[int64]time.Time

	// dueMu guards dueChecks: when each enabled check is next due for a probe,
	// keyed by check id. Process state like capPaged — a restart re-seeds and
	// the first sweep probes everything, which cold-start seeding absorbs.
	// Lazily initialized because New does not seed the map; checkDue fills it
	// on first use.
	dueMu     sync.Mutex
	dueChecks map[int64]time.Time

	mu      sync.RWMutex
	baseURL string
}

// New returns an Engine.
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("reactive: no store")
	}
	if opts.Queue == nil {
		return nil, errors.New("reactive: no queue")
	}
	if opts.Deriver == nil {
		return nil, errors.New("reactive: no deriver")
	}

	e := &Engine{
		store:       opts.Store,
		queue:       opts.Queue,
		deriver:     opts.Deriver,
		notifier:    opts.Notifier,
		playbooks:   opts.Playbooks,
		dedupWindow: opts.DedupWindow,
		approvalTTL: opts.ApprovalTTL,
		redactor:    opts.Redactor,
		clock:       opts.Clock,
		log:         opts.Logger,
		republished: map[int64]bool{},
		capPaged:    map[int64]time.Time{},
		baseURL:     opts.BaseURL,
	}
	if e.dedupWindow == 0 {
		e.dedupWindow = DefaultDedupWindow
	}
	if e.approvalTTL <= 0 {
		e.approvalTTL = DefaultApprovalTTL
	}
	if e.clock == nil {
		e.clock = func() time.Time { return time.Now().UTC() }
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	// Register as the queue's step-approver so a Sequencer built over the same
	// queue arms a gated workflow step through this same approval machinery. The
	// queue is passed as the Gate interface; the concrete daemon queue also
	// implements the setter, so this wires the daemon with no change at its call
	// site. A queue that does not (a pure test Gate) simply is not registered.
	if setter, ok := opts.Queue.(interface {
		SetStepApprover(queue.StepApprover)
	}); ok {
		setter.SetStepApprover(e)
	}
	return e, nil
}

// ArmStepApproval creates the pending approval a gated composed-chain step waits
// on, so the step is released only by the same DecideApproval path a single
// destructive apply uses. It is the queue.StepApprover seam: the Sequencer calls
// it when a require-approval step becomes current, and this mints the capability,
// records the approval with a TTL starting now, and pages the operator.
//
// It is idempotent on the job: an approval already recorded for it is a no-op,
// so the Sequencer re-checking a parked step each drive pass mints and notifies
// exactly once. The TTL therefore starts when the step becomes current, not at
// chain start — a slow predecessor cannot burn the window before the step is
// even reached.
func (e *Engine) ArmStepApproval(ctx context.Context, jobID int64) error {
	if _, err := e.store.ApprovalByJob(ctx, jobID); err == nil {
		return nil // already armed; do not re-mint or re-page
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	token, hash, err := ApprovalToken(e.deriver, jobID)
	if err != nil {
		return err
	}
	now := e.now()
	approval, err := e.store.CreateApproval(ctx, store.Approval{
		JobID:       jobID,
		RequestedAt: now,
		ExpiresAt:   now.Add(e.approvalTTL),
		TokenHash:   hash,
	})
	if err != nil {
		return err
	}
	e.notify(ctx, e.approvalNotification(approval, token, 1))
	return nil
}

func (e *Engine) now() time.Time { return e.clock().UTC() }

// SetBaseURL sets what decision links are rooted at. The daemon calls it once
// its listener's address is known and before it starts serving, because the
// address is only decided by the bind.
func (e *Engine) SetBaseURL(u string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.baseURL = u
}

func (e *Engine) base() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.baseURL
}

// notify publishes without letting delivery decide anything. A phone that is
// unreachable does not change what the ledgers say happened.
func (e *Engine) notify(ctx context.Context, note notify.Notification) {
	if e.notifier == nil {
		return
	}
	if err := e.notifier.Notify(ctx, note); err != nil {
		e.log.Error("reactive: notify", "title", note.Title, "err", err)
	}
}

// Ingest runs one normalized event through the pipeline and records what
// happened to it. It returns the stored row, so the ingress can answer the
// sender with the outcome rather than a bare 202.
//
// Every path through this function ends in exactly one event row: an event
// that vanished because a guardrail swallowed it is the failure mode the
// events table exists to prevent.
func (e *Engine) Ingest(ctx context.Context, ev Event, raw []byte) (store.Event, error) {
	var trace Trace

	// An alert that cleared cancels what its firing raised, before anything
	// else looks at it: approving a restart for a host that came back is the
	// one outcome nobody wants (user-feedback: resolve-cancels-approval).
	if ev.Resolved() {
		withdrawn, err := e.withdrawResolved(ctx, ev, &trace)
		if err != nil {
			return e.record(ctx, ev, raw, store.OutcomeError, "", err.Error(), nil, trace)
		}
		if withdrawn > 0 {
			return e.record(ctx, ev, raw, store.OutcomeMatched, "",
				fmt.Sprintf("resolved: withdrew %d pending approval(s)", withdrawn), nil, trace)
		}
	}

	set, err := LoadRules(ctx, e.store)
	if err != nil {
		return e.record(ctx, ev, raw, store.OutcomeError, "", err.Error(), nil, trace)
	}
	for name, compileErr := range set.Broken() {
		e.log.Error("reactive: rule will not compile", "rule", name, "err", compileErr)
		trace.add(StepRule, name, false, "will not compile: %v", compileErr)
	}

	rule, matched, err := e.match(ctx, ev, set, &trace)
	if err != nil {
		return e.record(ctx, ev, raw, store.OutcomeError, "", err.Error(), nil, trace)
	}
	if !matched {
		return e.record(ctx, ev, raw, store.OutcomeIgnored, "", "no rule matched", nil, trace)
	}

	verdict, err := e.runGuards(ctx, ev, rule, &trace)
	if err != nil {
		return e.record(ctx, ev, raw, store.OutcomeError, rule.Name, err.Error(), nil, trace)
	}
	if verdict.suppressed {
		return e.record(ctx, ev, raw, store.OutcomeSuppressed, rule.Name, verdict.reason, nil, trace)
	}

	return e.act(ctx, ev, raw, rule, &trace)
}

// match finds the first enabled rule whose source filter admits the event and
// whose expression says yes. Rules are in name order, so which one wins a tie
// is readable off `rule list`.
func (e *Engine) match(ctx context.Context, ev Event, set *Ruleset, trace *Trace) (store.Rule, bool, error) {
	for _, r := range set.Rules() {
		if !Applies(r, ev) {
			trace.add(StepRule, r.Name, false, "source filter %q does not admit %q",
				r.SourceFilter, ev.Source)
			continue
		}
		prg := set.Program(r.ID)
		if prg == nil {
			continue
		}
		ok, err := prg.Match(ctx, ev)
		if err != nil {
			// An expression that will not evaluate does not match. A rule that
			// cannot decide must not be able to fire an apply.
			e.log.Error("reactive: rule evaluation failed", "rule", r.Name, "err", err)
			trace.add(StepRule, r.Name, false, "evaluation failed: %v", err)
			continue
		}
		if !ok {
			trace.add(StepRule, r.Name, false, "expression did not match")
			continue
		}
		trace.add(StepRule, r.Name, true, "matched %s", r.CELExpr)
		return r, true, nil
	}
	return store.Rule{}, false, nil
}

// act turns a rule that survived the guardrails into a job, an approval, or a
// notification, and stamps the fire ledger the cooldown and the cap read.
func (e *Engine) act(ctx context.Context, ev Event, raw []byte, rule store.Rule, trace *Trace) (store.Event, error) {
	if rule.ActionKind == store.ActionNotify {
		trace.add(StepAction, rule.ActionKind, true, "notify only; no job")
		if _, err := e.store.RecordRuleFire(ctx, rule.ID, nil, ev.DedupKey); err != nil {
			return store.Event{}, err
		}
		e.notify(ctx, matchedNotification(rule, ev))
		return e.record(ctx, ev, raw, store.OutcomeMatched, rule.Name,
			"notify only; no job", nil, *trace)
	}

	spec, plan, err := e.buildSpec(ctx, ev, rule)
	if err != nil {
		trace.add(StepAction, rule.ActionKind, false, "%v", err)
		return e.record(ctx, ev, raw, store.OutcomeError, rule.Name, err.Error(), nil, *trace)
	}

	gate := rule.RequireApproval || Destructive(rule.ActionKind)
	if !gate {
		trace.add(StepApproval, rule.ActionKind, true, "not destructive and approval not required")
		job, err := e.queue.Enqueue(ctx, spec)
		if err != nil {
			return store.Event{}, err
		}
		if _, err := e.store.RecordRuleFire(ctx, rule.ID, &job.ID, ev.DedupKey); err != nil {
			return store.Event{}, err
		}
		return e.record(ctx, ev, raw, store.OutcomeFired, rule.Name,
			fmt.Sprintf("job %d queued", job.ID), &job.ID, *trace)
	}

	why := "rule requires approval"
	if Destructive(rule.ActionKind) {
		why = "action " + rule.ActionKind + " is destructive"
	}
	return e.gateAction(ctx, ev, raw, rule, spec, plan, why, trace)
}

// gateAction creates the job in awaiting_approval and the approval that can
// release it, then asks the human.
func (e *Engine) gateAction(ctx context.Context, ev Event, raw []byte, rule store.Rule,
	spec queue.Spec, plan planRef, why string, trace *Trace) (store.Event, error) {
	job, err := e.queue.EnqueueGated(ctx, spec)
	if err != nil {
		return store.Event{}, err
	}
	token, hash, err := ApprovalToken(e.deriver, job.ID)
	if err != nil {
		return store.Event{}, err
	}

	now := e.now()
	approval, err := e.store.CreateApproval(ctx, store.Approval{
		JobID:        job.ID,
		PlanArtifact: plan.artifact,
		Workspace:    plan.workspace,
		PlanSerial:   plan.serial,
		RequestedAt:  now,
		ExpiresAt:    now.Add(e.approvalTTL),
		TokenHash:    hash,
	})
	if err != nil {
		return store.Event{}, err
	}
	if _, err := e.store.RecordRuleFire(ctx, rule.ID, &job.ID, ev.DedupKey); err != nil {
		return store.Event{}, err
	}

	trace.add(StepApproval, rule.ActionKind, false, "%s; approval %d, expires %s",
		why, approval.ID, clock(approval.ExpiresAt))
	e.notify(ctx, e.approvalNotification(approval, token, 1))

	return e.record(ctx, ev, raw, store.OutcomeGated, rule.Name,
		fmt.Sprintf("approval %d pending, expires %s", approval.ID, clock(approval.ExpiresAt)),
		&job.ID, *trace)
}

// withdrawResolved cancels the approvals a now-cleared condition raised, but
// only the ones this resolve has the authority to cancel. A bare resolve
// carries no approval credential, so its authority is the rule that raised the
// approval: the resolve must be admitted by that rule and match it as a firing.
// A forged resolve that shares only the dedup key — stripped of the labels the
// rule fired on — cannot cancel a remediation it never had a rule for.
func (e *Engine) withdrawResolved(ctx context.Context, ev Event, trace *Trace) (int, error) {
	pending, err := e.store.PendingApprovalsByDedupKey(ctx, ev.DedupKey)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		trace.add(StepResolve, ev.DedupKey, true, "resolved; no approval was waiting on it")
		return 0, nil
	}

	withdrawn := 0
	for _, a := range pending {
		ok, why, err := e.resolveAuthorized(ctx, ev, a)
		if err != nil {
			return withdrawn, err
		}
		if !ok {
			trace.add(StepResolve, ev.DedupKey, false,
				"resolve refused for approval %d: %s", a.ID, why)
			continue
		}
		if _, err := e.store.DecideApproval(ctx, a.ID, store.ApprovalWithdrawn, "resolve",
			"the alert resolved while the approval was pending"); err != nil {
			if errors.Is(err, store.ErrApprovalDecided) {
				continue
			}
			return withdrawn, err
		}
		if err := e.queue.Withdraw(ctx, a.JobID); err != nil {
			return withdrawn, err
		}
		trace.add(StepResolve, ev.DedupKey, false,
			"resolved; approval %d withdrawn and job %d cancelled", a.ID, a.JobID)
		e.notify(ctx, withdrawnNotification(a))
		withdrawn++
	}
	return withdrawn, nil
}

// resolveAuthorized reports whether a resolve event may withdraw one pending
// approval. The authority is the rule that raised the approval: the resolve
// must come from the same source, be admitted by that rule's source filter, and
// match the rule when its status is read as firing — i.e. it is the same
// condition clearing, not an unrelated event that merely shares the dedup key.
// A rule that no longer exists or will not compile authorizes nothing; the
// approval then fails closed at its TTL rather than being cancelled by a
// resolve that cannot be scoped.
func (e *Engine) resolveAuthorized(ctx context.Context, ev Event, a store.Approval) (bool, string, error) {
	origin, err := e.store.EventByJobID(ctx, a.JobID)
	if errors.Is(err, store.ErrNotFound) {
		return false, "no event raised this approval", nil
	}
	if err != nil {
		return false, "", err
	}
	if origin.MatchedRule == "" {
		return false, "the approval was not raised by a rule", nil
	}
	if origin.Source != ev.Source {
		return false, "resolve source does not match the source that raised it", nil
	}

	rule, err := e.store.GetRule(ctx, origin.MatchedRule)
	if errors.Is(err, store.ErrNotFound) {
		return false, "the rule that raised it no longer exists", nil
	}
	if err != nil {
		return false, "", err
	}
	if !Applies(rule, ev) {
		return false, "the rule that raised it does not admit this source", nil
	}

	prg, err := Compile(rule.CELExpr)
	if err != nil {
		return false, "the rule that raised it no longer compiles", nil
	}
	probe := ev
	probe.Status = StatusFiring
	matched, err := prg.Match(ctx, probe)
	if err != nil {
		return false, "the rule that raised it could not evaluate the resolve", nil
	}
	if !matched {
		return false, "the resolve does not match the condition the rule fired on", nil
	}
	return true, "", nil
}

// record writes the one event row this ingestion produces, scrubbing sealed
// secret values out of both bodies on the way in: job_event and event are
// durable and append-only in spirit, so redaction happens before the write.
func (e *Engine) record(ctx context.Context, ev Event, raw []byte, outcome store.EventOutcome,
	ruleName, reason string, jobID *int64, trace Trace) (store.Event, error) {
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = e.now()
	}
	r := e.scrubber()
	return e.store.CreateEvent(ctx, store.Event{
		Source:      ev.Source,
		ReceivedAt:  ev.ReceivedAt,
		DedupKey:    ev.DedupKey,
		Normalized:  scrub(r, ev.JSON()),
		Raw:         scrub(r, string(raw)),
		MatchedRule: ruleName,
		Outcome:     outcome,
		Reason:      scrub(r, reason),
		JobID:       jobID,
		Trace:       scrub(r, trace.JSON()),
	})
}

// scrubber resolves the redactor for one recorded event, or nil when none is
// configured.
func (e *Engine) scrubber() Redactor {
	if e.redactor == nil {
		return nil
	}
	return e.redactor()
}

func scrub(r Redactor, s string) string {
	if r == nil || s == "" {
		return s
	}
	return string(r.Redact([]byte(s)))
}
