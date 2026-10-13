package reactive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// Decision outcomes. They are what the decision endpoint renders and what the
// CLI prints; a used or expired link is a terminal notice, never an error
// (ui-brief, approval surface contract 5).
const (
	DecisionOutcomeApproved = "approved"
	DecisionOutcomeRejected = "rejected"
	DecisionOutcomeSettled  = "already-decided"
	DecisionOutcomeExpired  = "expired"
	DecisionOutcomeStale    = "stale-plan"
)

// ErrStalePlan reports that the workspace moved after the plan under decision
// was made. It is a refusal, not a disabled button: the operator re-plans or
// discards (ui-brief, approval surface contract 4).
var ErrStalePlan = errors.New("reactive: the plan is stale")

// SourceApproval attributes every notification about one approval — the opening
// page and its reminder, and the resolve that closes it — to a single stable
// identity, so a transport that models incidents (GoAlert) closes the incident
// the page opened. The per-approval Host below makes each incident distinct.
const SourceApproval = "approval"

// approvalHost scopes the notification identity to one approval, so its resolve
// closes exactly the incident its page opened and no other. It is the approval
// id, never a hostname or anything the lock-screen contract withholds.
func approvalHost(a store.Approval) string { return fmt.Sprintf("%d", a.ID) }

// approvalClick is the deep link every notification about one approval opens:
// an id, nothing more — the same withholding the lock screen already applies
// to Title and Body (ui-brief, approval surface contract 1).
func approvalClick(a store.Approval) string {
	return fmt.Sprintf("exocomp://approvals/%d", a.ID)
}

// approvalIDTag is the structured id tag every approval notification carries,
// so a subscriber renders "approval 42" without parsing the capability URL.
func approvalIDTag(a store.Approval) string {
	return fmt.Sprintf("id:%d", a.ID)
}

// Decision is what happened to a decision link.
type Decision struct {
	Approval store.Approval
	Outcome  string
	// Message is the terminal notice: one paragraph a phone can read without
	// naming anything a notification would not.
	Message string
}

// Decide resolves one decision link. It is the single place a human's answer
// enters the system, whatever surface asked: HTTP callback, CLI, or TUI.
//
// The order is the fail-closed one: the token first, then whether the question
// is still open, then the TTL, then the stale-plan check — so an expired link
// can never release a job, and a stale plan is refused before the approval is
// spent rather than after.
func (e *Engine) Decide(ctx context.Context, approvalID int64, decision, token, via string) (Decision, error) {
	if decision != DecisionApprove && decision != DecisionReject {
		return Decision{}, fmt.Errorf("reactive: %q is not %s or %s",
			decision, DecisionApprove, DecisionReject)
	}

	approval, err := e.store.GetApproval(ctx, approvalID)
	if err != nil {
		return Decision{}, err
	}
	if err := VerifyApprovalToken(e.deriver, approval.JobID, approval.TokenHash, token); err != nil {
		return Decision{}, err
	}

	if approval.State != store.ApprovalPending {
		// Already settled: close its incident, in case the page's opener never
		// saw the transitions that settled it (a settle recorded on another path,
		// or a delivery that failed then). The close is idempotent.
		e.notify(ctx, decidedNotification(approval, DecisionOutcomeSettled))
		return Decision{
			Approval: approval,
			Outcome:  DecisionOutcomeSettled,
			Message:  settledNotice(approval),
		}, nil
	}

	if !e.now().Before(approval.ExpiresAt) {
		expired, err := e.expire(ctx, approval)
		if err != nil {
			return Decision{}, err
		}
		return Decision{
			Approval: expired,
			Outcome:  DecisionOutcomeExpired,
			Message: fmt.Sprintf("This decision expired at %s. The job was rejected and "+
				"nothing was applied.", clock(approval.ExpiresAt)),
		}, nil
	}

	if decision == DecisionApprove {
		if notice, stale, err := e.stalePlan(ctx, approval); err != nil {
			return Decision{}, err
		} else if stale {
			// Deliberately not spent: discarding is a reject on the same link,
			// and burning the token here would leave the operator with neither
			// option.
			return Decision{
				Approval: approval,
				Outcome:  DecisionOutcomeStale,
				Message:  notice,
			}, ErrStalePlan
		}
	}

	state := store.ApprovalApproved
	outcome := DecisionOutcomeApproved
	if decision == DecisionReject {
		state = store.ApprovalRejected
		outcome = DecisionOutcomeRejected
	}

	decided, err := e.store.DecideApproval(ctx, approvalID, state, via, "")
	if errors.Is(err, store.ErrApprovalDecided) {
		// Two taps raced; the loser reports what the winner decided and closes the
		// incident, redundantly with the winner — the close is idempotent.
		e.notify(ctx, decidedNotification(decided, DecisionOutcomeSettled))
		return Decision{
			Approval: decided,
			Outcome:  DecisionOutcomeSettled,
			Message:  settledNotice(decided),
		}, nil
	}
	if err != nil {
		return Decision{}, err
	}

	// DecideApproval has committed the human's answer; the queue transition below
	// is the second, separate write. If it fails or the process dies between the
	// two, the approval is terminal while the job is still awaiting_approval — a
	// crash-window inconsistency. The store's ReconcileNonTerminal is the
	// backstop: it finds the settled approval on its next pass and applies the
	// same transition, so the job is never stranded even though this call
	// returns the error.
	if state == store.ApprovalApproved {
		if err := e.queue.Release(ctx, decided.JobID); err != nil {
			return Decision{}, err
		}
	} else if err := e.queue.Withdraw(ctx, decided.JobID); err != nil {
		return Decision{}, err
	}

	e.notify(ctx, decidedNotification(decided, outcome))
	return Decision{
		Approval: decided,
		Outcome:  outcome,
		Message:  fmt.Sprintf("Recorded: %s. Job %d %s.", outcome, decided.JobID, jobFate(state)),
	}, nil
}

func jobFate(state store.ApprovalState) string {
	if state == store.ApprovalApproved {
		return "is queued and will run"
	}
	return "was cancelled; nothing was applied"
}

func settledNotice(a store.Approval) string {
	when := ""
	if a.DecidedAt != nil {
		when = " at " + clock(*a.DecidedAt)
	}
	return fmt.Sprintf("This decision link has already been used: %s%s. Nothing changed.",
		a.State, when)
}

// stalePlan reports whether the workspace moved after the plan under decision
// was made, and renders what moved it. The attribution matters more than the
// refusal: "which job moved it and from where" is what tells the operator
// whether re-planning is safe.
func (e *Engine) stalePlan(ctx context.Context, a store.Approval) (string, bool, error) {
	if a.Workspace == "" {
		return "", false, nil
	}
	ws, err := e.store.WorkspaceByName(ctx, a.Workspace)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if ws.Serial <= a.PlanSerial {
		return "", false, nil
	}

	mover, err := e.movedBy(ctx, ws.ID, a.PlanSerial)
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("Refused: the plan was made at serial %d and the workspace is now at "+
		"serial %d, %s. The plan is kept but cannot be applied — re-plan, or reject this to "+
		"discard it. Nothing was applied.",
		a.PlanSerial, ws.Serial, mover), true, nil
}

// movedBy names the job that first advanced the workspace past a serial, and
// what triggered it.
func (e *Engine) movedBy(ctx context.Context, workspaceID, since int64) (string, error) {
	versions, err := e.store.ListTFStates(ctx, workspaceID, 0)
	if err != nil {
		return "", err
	}

	// Newest first; the last one above the plan's serial is the one that
	// moved it first.
	var first *store.TFState
	for i := range versions {
		if versions[i].Serial > since {
			first = &versions[i]
		}
	}
	if first == nil || first.CreatedByJob == nil {
		return "moved by something outside this control plane", nil
	}

	job, err := e.store.GetJob(ctx, *first.CreatedByJob)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Sprintf("moved by job %d, which no longer exists", *first.CreatedByJob), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("moved by job %d (%s, triggered %s) at %s",
		job.ID, job.Runbook, job.Trigger, clock(first.CreatedAt)), nil
}

// expire fails an approval closed: the TTL running out rejects the job rather
// than leaving it claimable. An unreachable daemon cannot hurt you because the
// default is refusal, not eventual execution.
func (e *Engine) expire(ctx context.Context, a store.Approval) (store.Approval, error) {
	expired, err := e.store.DecideApproval(ctx, a.ID, store.ApprovalExpired, "ttl",
		"the decision window closed with no answer")
	if errors.Is(err, store.ErrApprovalDecided) {
		return expired, nil
	}
	if err != nil {
		return store.Approval{}, err
	}
	if err := e.queue.Withdraw(ctx, expired.JobID); err != nil {
		return store.Approval{}, err
	}
	e.notify(ctx, expiredNotification(expired))
	return expired, nil
}

// SweepApprovals runs the two clock-driven halves of the approval surface:
// every TTL that ran out fails closed, and every approval past half its window
// gets one more push. It is called on a timer by the daemon.
func (e *Engine) SweepApprovals(ctx context.Context) (expired, republished int, err error) {
	now := e.now()

	due, err := e.store.PendingApprovalsExpiredAt(ctx, now)
	if err != nil {
		return 0, 0, err // structural: the sweep cannot run at all
	}
	for _, a := range due {
		_, keep := e.sweepStep(ctx, "sweep approvals", fmt.Sprintf("approval %d", a.ID), func() (int, error) {
			if _, err := e.expire(ctx, a); err != nil {
				return 0, err
			}
			expired++
			return 1, nil
		})
		if !keep {
			return expired, republished, ctx.Err()
		}
	}

	pending, err := e.store.ListApprovals(ctx, true)
	if err != nil {
		return expired, 0, err // structural: the sweep cannot run at all
	}
	e.pruneRepublished(pending)
	for _, a := range pending {
		half := a.RequestedAt.Add(a.ExpiresAt.Sub(a.RequestedAt) / 2)
		if now.Before(half) || e.republished[a.ID] {
			continue
		}
		_, keep := e.sweepStep(ctx, "sweep approvals", fmt.Sprintf("approval %d", a.ID), func() (int, error) {
			token, _, err := ApprovalToken(e.deriver, a.JobID)
			if err != nil {
				return 0, err
			}
			e.notify(ctx, e.approvalNotification(ctx, a, token, 2))
			// The mark stays inside the closure: a tolerated token or notify
			// error must leave the approval unmarked, so the next sweep retries
			// the reminder instead of suppressing it forever.
			e.republished[a.ID] = true
			republished++
			return 1, nil
		})
		if !keep {
			return expired, republished, ctx.Err()
		}
	}
	return expired, republished, nil
}

// pruneRepublished evicts the reminder-sent marker for any approval no longer
// pending — decided, withdrawn, or expired — so republished stays bounded by
// the current pending count instead of growing across the daemon's lifetime.
func (e *Engine) pruneRepublished(pending []store.Approval) {
	live := make(map[int64]bool, len(pending))
	for _, a := range pending {
		live[a.ID] = true
	}
	for id := range e.republished {
		if !live[id] {
			delete(e.republished, id)
		}
	}
}

// DecisionLinks returns the two URLs for a pending approval, for the CLI and
// the TUI to print. They are capabilities: printing one is handing it out.
func (e *Engine) DecisionLinks(a store.Approval) (approve, reject string, err error) {
	token, _, err := ApprovalToken(e.deriver, a.JobID)
	if err != nil {
		return "", "", err
	}
	base := e.base()
	return DecisionURL(base, a.ID, DecisionApprove, token),
		DecisionURL(base, a.ID, DecisionReject, token), nil
}

// approvalNotification is the lock-screen message: a count, a TTL, and what to
// do about it. At the full detail rung it also names what it wants to do (the
// workspace apply, or the runbook a step gates); the transport drops that line
// at the summary and terse rungs. It never names a plan, a diff, or a resource
// beyond the workspace — what the job will actually change stays behind the tap.
func (e *Engine) approvalNotification(ctx context.Context, a store.Approval, token string, attempt int) notify.Notification {
	title := "1 approval waiting"
	priority := notify.PriorityHigh
	if attempt > 1 {
		title = "1 approval still waiting"
		priority = notify.PriorityMax
	}
	left := humanDuration(a.ExpiresAt.Sub(e.now()))
	ttl := int64(a.ExpiresAt.Sub(e.now()).Seconds())
	if ttl < 0 {
		ttl = 0
	}
	base := e.base()

	body := fmt.Sprintf("%s left, then it auto-rejects — open to see what it wants to do.", left)
	if subject := e.approvalSubject(ctx, a); subject != "" {
		body = fmt.Sprintf("%s · %s left, then it auto-rejects.", subject, left)
	}

	return notify.Notification{
		Title:    title,
		Body:     body,
		Priority: priority,
		Tags:     []string{"warning", approvalIDTag(a), fmt.Sprintf("ttl:%d", ttl), "kind:approval"},
		Click:    approvalClick(a),
		Actions: []notify.Action{
			{Label: "Approve", URL: DecisionURL(base, a.ID, DecisionApprove, token)},
			{Label: "Reject", URL: DecisionURL(base, a.ID, DecisionReject, token)},
		},
		Source: SourceApproval,
		Host:   approvalHost(a),
	}
}

// approvalSubject names what an approval is about, for the full detail rung: a
// workspace apply (the plan-gated case, where the workspace is recorded), or the
// runbook a step gates. It names the resource only, and returns "" when neither
// is known — the notification then keeps its generic body.
func (e *Engine) approvalSubject(ctx context.Context, a store.Approval) string {
	if a.Workspace != "" {
		return "apply " + a.Workspace
	}
	job, err := e.store.GetJob(ctx, a.JobID)
	if err != nil || job.Runbook == "" {
		return ""
	}
	if ws, ok := WorkspaceOf(job.Runbook); ok {
		return "apply " + ws
	}
	return job.Runbook
}

// decidedNotification tells the operator an approval settled and, carrying the
// opening page's identity with Resolved set, closes the incident that page
// opened on an incident-modelling transport. A fire-and-forget transport still
// delivers it as an ordinary message.
func decidedNotification(a store.Approval, outcome string) notify.Notification {
	return notify.Notification{
		Title:    "Approval " + outcome,
		Body:     fmt.Sprintf("Approval %d was %s via %s.", a.ID, outcome, a.DecidedVia),
		Priority: notify.PriorityDefault,
		Tags:     []string{approvalIDTag(a), "kind:approval"},
		Click:    approvalClick(a),
		Source:   SourceApproval,
		Host:     approvalHost(a),
		Resolved: true,
	}
}

func expiredNotification(a store.Approval) notify.Notification {
	return notify.Notification{
		Title:    "Approval expired",
		Body:     fmt.Sprintf("Approval %d ran out its window and was rejected. Nothing was applied.", a.ID),
		Priority: notify.PriorityHigh,
		Tags:     []string{"hourglass", approvalIDTag(a), "kind:approval"},
		Click:    approvalClick(a),
		Source:   SourceApproval,
		Host:     approvalHost(a),
		Resolved: true,
	}
}

func withdrawnNotification(a store.Approval) notify.Notification {
	return notify.Notification{
		Title:    "Approval withdrawn",
		Body:     fmt.Sprintf("The condition cleared, so approval %d was withdrawn and its job cancelled.", a.ID),
		Priority: notify.PriorityDefault,
		Tags:     []string{approvalIDTag(a), "kind:approval"},
		Click:    approvalClick(a),
		Source:   SourceApproval,
		Host:     approvalHost(a),
		Resolved: true,
	}
}

// capThrottledNotification is max priority because a rule at its cap is either
// misfiring or under a flood, and both want a human now. It stays armed and
// keeps evaluating — the page says so, so nobody re-enables a rule that was
// never off.
func capThrottledNotification(rule store.Rule) notify.Notification {
	return notify.Notification{
		Title: "Rule hit its daily cap",
		Body: fmt.Sprintf("Rule %q hit its cap of %d in %s. It stays armed and keeps "+
			"evaluating; its actions are withheld until the rolling window clears. "+
			"A flood that trips this is worth looking at now.",
			rule.Name, rule.DailyCap, humanDuration(capWindow)),
		Priority: notify.PriorityMax,
		Tags:     []string{"rotating_light"},
	}
}

func matchedNotification(rule store.Rule, ev Event) notify.Notification {
	return notify.Notification{
		Title:    "Rule matched: " + rule.Name,
		Body:     fmt.Sprintf("%s %s", ev.Name, ev.Status),
		Priority: notify.PriorityDefault,
	}
}

// ApprovalDeadline renders how long a pending approval has left, for the
// CLI's and the TUI's listings. A decided one has no deadline: its window
// stopped mattering the moment it was answered.
func ApprovalDeadline(a store.Approval, now time.Time) string {
	switch {
	case a.State != store.ApprovalPending:
		return "-"
	case !now.Before(a.ExpiresAt):
		return "expired"
	default:
		return humanDuration(a.ExpiresAt.Sub(now)) + " left"
	}
}
