package reactive_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// gatedRule requires approval for an otherwise harmless shell action, which is
// how a rule opts in without needing a destructive kind.
func gatedRule(name string) store.NewRule {
	r := shellRule(name)
	r.RequireApproval = true
	return r
}

// gate ingests one firing alert against a rule that requires approval and
// returns the event, the approval, and the job the approval gates.
func (h *harness) gate(t *testing.T, alert, host string) (store.Event, store.Approval, store.Job) {
	t.Helper()
	ctx := context.Background()

	stored, err := h.engine.Ingest(ctx, h.firing(t, alert, host), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeGated {
		t.Fatalf("outcome = %s (%s), want gated", stored.Outcome, stored.Reason)
	}
	if stored.JobID == nil {
		t.Fatal("a gated event names no job")
	}
	job, err := h.store.GetJob(ctx, *stored.JobID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want awaiting_approval", job.State)
	}
	approval, err := h.store.ApprovalByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("approval by job: %v", err)
	}
	return stored, approval, job
}

func (h *harness) token(t *testing.T, jobID int64) string {
	t.Helper()
	token, _, err := reactive.ApprovalToken(h.deriver, jobID)
	if err != nil {
		t.Fatalf("approval token: %v", err)
	}
	return token
}

func TestApprovalHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	event, approval, job := h.gate(t, "DiskFull", "web-01")

	if !strings.HasPrefix(event.Reason, "approval "+strconv.FormatInt(approval.ID, 10)+" pending, expires ") {
		t.Fatalf("gated reason = %q", event.Reason)
	}

	// The notification is terse: a count and a TTL, no host or resource.
	if len(h.sent.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(h.sent.sent))
	}
	note := h.sent.sent[0]
	if note.Title != "1 approval waiting" {
		t.Fatalf("notification title = %q", note.Title)
	}
	for _, leak := range []string{"web-01", "DiskFull", "/bin/true", "restart"} {
		if strings.Contains(note.Title+note.Body, leak) {
			t.Fatalf("notification names %q: %q / %q", leak, note.Title, note.Body)
		}
	}
	if len(note.Actions) != 2 {
		t.Fatalf("notification carries %d actions, want approve and reject", len(note.Actions))
	}

	decision, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, h.token(t, job.ID), "cli")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decision.Outcome != reactive.DecisionOutcomeApproved {
		t.Fatalf("decision outcome = %s", decision.Outcome)
	}

	released, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if released.State != store.StateQueued {
		t.Fatalf("approved job is %s, want queued", released.State)
	}
}

// TestApprovalNotificationCarriesClickAndTags pins the structured extras a
// subscriber renders from instead of parsing the capability action URL: a
// deep link naming only the approval id, and tags naming the id and how long
// is left. The single-use decision token lives only in the Actions' URLs, and
// must never leak into either.
func TestApprovalNotificationCarriesClickAndTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t, harnessOptions{approvalTTL: 30 * time.Minute})
	h.addRule(t, gatedRule("restart"))
	_, approval, _ := h.gate(t, "DiskFull", "web-01")

	if len(h.sent.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(h.sent.sent))
	}
	note := h.sent.sent[0]

	wantClick := fmt.Sprintf("exocomp://approvals/%d", approval.ID)
	if note.Click != wantClick {
		t.Fatalf("click = %q, want %q", note.Click, wantClick)
	}

	wantIDTag := fmt.Sprintf("id:%d", approval.ID)
	if !slices.Contains(note.Tags, wantIDTag) {
		t.Fatalf("tags = %v, want %q", note.Tags, wantIDTag)
	}
	if !slices.ContainsFunc(note.Tags, func(tag string) bool { return strings.HasPrefix(tag, "ttl:") }) {
		t.Fatalf("tags = %v, want a ttl:<seconds> tag", note.Tags)
	}

	if len(note.Actions) != 2 {
		t.Fatalf("actions = %d, want approve and reject", len(note.Actions))
	}
	for _, action := range note.Actions {
		u, err := url.Parse(action.URL)
		if err != nil {
			t.Fatalf("parse action url %q: %v", action.URL, err)
		}
		token := u.Query().Get("t")
		if token == "" {
			t.Fatalf("action url %q carries no capability token to check against", action.URL)
		}
		if strings.Contains(note.Click, token) {
			t.Fatalf("click %q leaks the capability token", note.Click)
		}
		for _, tag := range note.Tags {
			if strings.Contains(tag, token) {
				t.Fatalf("tag %q leaks the capability token", tag)
			}
		}
	}
}

func TestApprovalRejectCancelsTheJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	decision, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionReject, h.token(t, job.ID), "cli")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if decision.Outcome != reactive.DecisionOutcomeRejected {
		t.Fatalf("decision outcome = %s", decision.Outcome)
	}
	rejected, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get rejected job: %v", err)
	}
	if rejected.State != store.StateCancelled {
		t.Fatalf("rejected job is %s, want cancelled", rejected.State)
	}
}

// TestApprovalLinkIsSingleUse: the second tap is a terminal notice, not an
// error and not a second decision.
func TestApprovalLinkIsSingleUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	_, approval, job := h.gate(t, "DiskFull", "web-01")
	token := h.token(t, job.ID)

	if _, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, token, "cli"); err != nil {
		t.Fatalf("first decision: %v", err)
	}

	// The same link, now asking for the opposite answer, must not flip it.
	second, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionReject, token, "cli")
	if err != nil {
		t.Fatalf("second decision: %v", err)
	}
	if second.Outcome != reactive.DecisionOutcomeSettled {
		t.Fatalf("second decision outcome = %s, want %s", second.Outcome, reactive.DecisionOutcomeSettled)
	}
	if !strings.Contains(second.Message, "already been used") {
		t.Fatalf("terminal notice = %q", second.Message)
	}
	if second.Approval.State != store.ApprovalApproved {
		t.Fatalf("approval is %s after a second tap, want approved", second.Approval.State)
	}
	job, err = h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateQueued {
		t.Fatalf("job is %s after a second tap, want queued", job.State)
	}
}

// TestForgedTokensAreRefused: a token for another job, a random string, and a
// missing one all fail, and none of them decides anything.
func TestForgedTokensAreRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"another job's token", h.token(t, job.ID+1)},
		{"a made-up token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"no token", ""},
	} {
		if _, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, tc.token, "cli"); !errors.Is(err, reactive.ErrBadToken) {
			t.Fatalf("%s: err = %v, want ErrBadToken", tc.name, err)
		}
	}

	still, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if still.State != store.ApprovalPending {
		t.Fatalf("approval is %s after forged tokens, want pending", still.State)
	}
}

// TestApprovalExpiresFailClosed: the TTL running out rejects the job. An
// unreachable operator cannot cause anything to run.
func TestApprovalExpiresFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{approvalTTL: 30 * time.Minute})
	h.addRule(t, gatedRule("restart"))
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	h.advance(31 * time.Minute)
	expired, republished, err := h.engine.SweepApprovals(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if expired != 1 {
		t.Fatalf("swept %d expired approvals, want 1", expired)
	}
	if republished != 0 {
		t.Fatalf("republished %d approvals that had already expired", republished)
	}

	settled, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if settled.State != store.ApprovalExpired {
		t.Fatalf("approval is %s, want expired", settled.State)
	}
	dead, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if dead.State != store.StateCancelled {
		t.Fatalf("job is %s after the TTL, want cancelled", dead.State)
	}

	// A link that arrives after the deadline is a terminal notice, and it
	// still cannot release the job.
	late, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, h.token(t, job.ID), "link")
	if err != nil {
		t.Fatalf("late decision: %v", err)
	}
	if late.Outcome != reactive.DecisionOutcomeSettled {
		t.Fatalf("late decision outcome = %s", late.Outcome)
	}
	dead, err = h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if dead.State != store.StateCancelled {
		t.Fatalf("job is %s after a late approval, want cancelled", dead.State)
	}
}

// TestExpiryIsCheckedOnTheDecisionPathToo: no sweep has run, and the link is
// still refused. The sweep is a convenience, not the guard.
func TestExpiryIsCheckedOnTheDecisionPathToo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{approvalTTL: 10 * time.Minute})
	h.addRule(t, gatedRule("restart"))
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	h.advance(11 * time.Minute)
	decision, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, h.token(t, job.ID), "link")
	if err != nil {
		t.Fatalf("decide past the TTL: %v", err)
	}
	if decision.Outcome != reactive.DecisionOutcomeExpired {
		t.Fatalf("decision outcome = %s, want expired", decision.Outcome)
	}
	dead, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if dead.State != store.StateCancelled {
		t.Fatalf("job is %s, want cancelled", dead.State)
	}
}

// TestRepublishAtHalfTheTTL: one more push, at max priority, once.
func TestRepublishAtHalfTheTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{approvalTTL: 30 * time.Minute})
	h.addRule(t, gatedRule("restart"))
	h.gate(t, "DiskFull", "web-01")

	h.advance(10 * time.Minute)
	if _, republished, err := h.engine.SweepApprovals(ctx); err != nil || republished != 0 {
		t.Fatalf("sweep before half the TTL: republished %d, err %v", republished, err)
	}

	h.advance(6 * time.Minute)
	_, republished, err := h.engine.SweepApprovals(ctx)
	if err != nil {
		t.Fatalf("sweep at half the TTL: %v", err)
	}
	if republished != 1 {
		t.Fatalf("republished %d, want 1", republished)
	}
	last := h.sent.sent[len(h.sent.sent)-1]
	if last.Priority != notify.PriorityMax || !strings.Contains(last.Title, "still waiting") {
		t.Fatalf("republished notification = %+v", last)
	}

	// It does not repeat on every later sweep.
	h.advance(time.Minute)
	if _, again, err := h.engine.SweepApprovals(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep republished %d, err %v", again, err)
	}
}

// TestResolveWithdrawsAPendingApproval: approving a restart for a host that
// came back on its own is the one outcome nobody wants.
func TestResolveWithdrawsAPendingApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	rule := gatedRule("restart")
	// The rule matches the firing alert only; the resolve arrives as its own
	// event and is handled before any rule looks at it.
	h.addRule(t, rule)
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	resolved, err := h.engine.Ingest(ctx, h.event(t, "DiskFull", "web-01", "resolved"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest resolve: %v", err)
	}
	if resolved.Outcome != store.OutcomeMatched {
		t.Fatalf("resolve outcome = %s (%s), want matched", resolved.Outcome, resolved.Reason)
	}
	if !strings.Contains(resolved.Reason, "withdrew 1 pending approval") {
		t.Fatalf("resolve reason = %q", resolved.Reason)
	}

	withdrawn, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if withdrawn.State != store.ApprovalWithdrawn {
		t.Fatalf("approval is %s, want withdrawn", withdrawn.State)
	}
	cancelled, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if cancelled.State != store.StateCancelled {
		t.Fatalf("job is %s after the resolve, want cancelled", cancelled.State)
	}

	// The link that was sent out cannot resurrect it.
	late, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove, h.token(t, job.ID), "link")
	if err != nil {
		t.Fatalf("decide after the withdrawal: %v", err)
	}
	if late.Outcome != reactive.DecisionOutcomeSettled {
		t.Fatalf("outcome after the withdrawal = %s", late.Outcome)
	}
}

// TestForgedResolveCannotWithdrawAnApproval proves that a
// resolve event withdraws the approvals its firing raised, and the old path did
// so on the dedup key alone, before any rule looked at it. An attacker who can
// reach the hook forges a "resolved" that shares the key but carries none of
// the labels the rule fired on, and the remediation is cancelled with no
// approval credential and no matching rule. The withdrawal must be scoped to
// the rule that raised the approval: a resolve that would not have matched that
// rule as a firing has no authority to cancel its job.
func TestForgedResolveCannotWithdrawAnApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	// The rule fires only on a critical DiskFull and requires approval.
	h.addRule(t, store.NewRule{
		Name:            "restart",
		CELExpr:         `event.name == "DiskFull" && event.severity == "critical"`,
		ActionKind:      store.ActionShell,
		Action:          store.RuleAction{Argv: []string{"/bin/true"}},
		RequireApproval: true,
	})

	const key = "alertmanager:fp:cond-1"
	firing := reactive.Event{
		Source: reactive.SourceAlertmanager, Name: "DiskFull", Severity: "critical",
		Host: "web-01", Status: "firing", DedupKey: key, ReceivedAt: h.nowValue,
	}
	gated, err := h.engine.Ingest(ctx, firing, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest firing: %v", err)
	}
	if gated.Outcome != store.OutcomeGated || gated.JobID == nil {
		t.Fatalf("firing outcome = %s (%s), want gated with a job", gated.Outcome, gated.Reason)
	}
	approval, err := h.store.ApprovalByJob(ctx, *gated.JobID)
	if err != nil {
		t.Fatalf("approval by job: %v", err)
	}

	// The forgery: same source and dedup key, status resolved, but stripped of
	// the severity the rule fired on — it matches no rule.
	forged := reactive.Event{
		Source: reactive.SourceAlertmanager, Name: "DiskFull",
		Host: "web-01", Status: "resolved", DedupKey: key, ReceivedAt: h.nowValue,
	}
	if _, err := h.engine.Ingest(ctx, forged, []byte(`{}`)); err != nil {
		t.Fatalf("ingest forged resolve: %v", err)
	}

	// The approval is untouched and the job is still awaiting the human.
	after, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if after.State != store.ApprovalPending {
		t.Fatalf("approval is %s after a forged resolve, want still pending", after.State)
	}
	job, err := h.store.GetJob(ctx, *gated.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("job is %s after a forged resolve, want still awaiting_approval", job.State)
	}
}

// TestStalePlanIsRefusedWithAttribution: the workspace moved after the plan,
// so approving is refused — and the refusal says what moved it, which job, and
// from where.
func TestStalePlanIsRefusedWithAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	ws, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "prod", TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	h.addRule(t, store.NewRule{
		Name:       "apply-on-drift",
		CELExpr:    `event.name == "DiskFull"`,
		ActionKind: store.ActionTofuApply,
		Action:     store.RuleAction{Workspace: "prod"},
	})

	stored, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// A destructive action is gated even though the rule never asked for it.
	if stored.Outcome != store.OutcomeGated {
		t.Fatalf("outcome = %s (%s), want gated", stored.Outcome, stored.Reason)
	}
	approval, err := h.store.ApprovalByJob(ctx, *stored.JobID)
	if err != nil {
		t.Fatalf("approval by job: %v", err)
	}
	if approval.Workspace != "prod" || approval.PlanSerial != 0 {
		t.Fatalf("approval = %+v, want it pinned to prod at serial 0", approval)
	}

	// Something else applies meanwhile, and the workspace's serial moves.
	mover, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: reactive.WorkspaceRunbook("prod"), Kind: "tofu", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"tofu", "apply"}},
	})
	if err != nil {
		t.Fatalf("create mover job: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 7, JobID: &mover.ID, Ciphertext: []byte("cipher"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}

	decision, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove,
		h.token(t, *stored.JobID), "link")
	if !errors.Is(err, reactive.ErrStalePlan) {
		t.Fatalf("approve a stale plan: err = %v, want ErrStalePlan", err)
	}
	if decision.Outcome != reactive.DecisionOutcomeStale {
		t.Fatalf("decision outcome = %s", decision.Outcome)
	}
	for _, want := range []string{
		"serial 0", "serial 7",
		"moved by job " + strconv.FormatInt(mover.ID, 10),
		"workspace:prod", "triggered manual", "Nothing was applied",
	} {
		if !strings.Contains(decision.Message, want) {
			t.Fatalf("refusal %q does not name %q", decision.Message, want)
		}
	}

	// The refusal did not spend the approval: discarding is still possible on
	// the same link, and that is the only other move offered.
	still, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if still.State != store.ApprovalPending {
		t.Fatalf("approval is %s after a stale refusal, want pending", still.State)
	}
	discard, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionReject,
		h.token(t, *stored.JobID), "link")
	if err != nil {
		t.Fatalf("discard a stale plan: %v", err)
	}
	if discard.Outcome != reactive.DecisionOutcomeRejected {
		t.Fatalf("discard outcome = %s", discard.Outcome)
	}
}

// TestFreshPlanIsNotStale: the same workspace, unmoved, approves normally.
func TestFreshPlanIsNotStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	if _, err := h.store.CreateWorkspace(ctx, store.Workspace{
		Name: "prod", TofuDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	h.addRule(t, store.NewRule{
		Name:       "apply-on-drift",
		CELExpr:    `event.name == "DiskFull"`,
		ActionKind: store.ActionTofuApply,
		Action:     store.RuleAction{Workspace: "prod"},
	})

	stored, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	approval, err := h.store.ApprovalByJob(ctx, *stored.JobID)
	if err != nil {
		t.Fatalf("approval by job: %v", err)
	}

	decision, err := h.engine.Decide(ctx, approval.ID, reactive.DecisionApprove,
		h.token(t, *stored.JobID), "link")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decision.Outcome != reactive.DecisionOutcomeApproved {
		t.Fatalf("decision outcome = %s", decision.Outcome)
	}
}

// TestDecisionEndpointOverHTTP drives the same paths through the URL a phone
// taps, which is the surface the contract is written about.
func TestDecisionEndpointOverHTTP(t *testing.T) {
	t.Parallel()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	srv := h.serve(t, "")
	_, approval, job := h.gate(t, "DiskFull", "web-01")

	base := srv.URL
	token := h.token(t, job.ID)

	// A wrong token and a nonexistent approval answer the same way.
	code, body := get(t, srv.Client(), reactive.DecisionURL(base, approval.ID, reactive.DecisionApprove, "nope"))
	if code != 403 {
		t.Fatalf("wrong token answered %d: %s", code, body)
	}
	code, _ = get(t, srv.Client(), reactive.DecisionURL(base, approval.ID+999, reactive.DecisionApprove, token))
	if code != 403 {
		t.Fatalf("unknown approval answered %d, want the same 403 a wrong token gets", code)
	}

	code, body = get(t, srv.Client(), reactive.DecisionURL(base, approval.ID, reactive.DecisionApprove, token))
	if code != 200 {
		t.Fatalf("approve answered %d: %s", code, body)
	}
	if !strings.Contains(body, `"outcome":"approved"`) {
		t.Fatalf("approve body = %s", body)
	}

	// The reused link is a terminal notice with a 200, not an error.
	code, body = get(t, srv.Client(), reactive.DecisionURL(base, approval.ID, reactive.DecisionReject, token))
	if code != 200 || !strings.Contains(body, "already been used") {
		t.Fatalf("reused link answered %d: %s", code, body)
	}
}
