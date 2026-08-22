package reactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// The steps of an evaluation trace, in the order they run. They are the
// column of the events screen's trace view, so the names are the vocabulary
// the operator reads.
const (
	StepResolve  = "resolve"
	StepRule     = "rule"
	StepSilence  = "silence"
	StepDedup    = "dedup"
	StepCooldown = "cooldown"
	StepCap      = "cap"
	StepApproval = "approval"
	StepAction   = "action"
)

// Condition is one line of an event's evaluation trace: what was asked, what
// the ledger answered, and whether the event survived it.
type Condition struct {
	Step   string `json:"step"`
	Detail string `json:"detail,omitempty"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}

// Trace is an event's per-condition evaluation record.
type Trace []Condition

// JSON renders a trace as the trace_json column holds it.
func (t Trace) JSON() string {
	if len(t) == 0 {
		return "[]"
	}
	blob, err := json.Marshal(t)
	if err != nil {
		return "[]"
	}
	return string(blob)
}

// DecodeTrace reads back a stored trace.
func DecodeTrace(raw string) (Trace, error) {
	if raw == "" {
		return nil, nil
	}
	var t Trace
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("reactive: decode trace: %w", err)
	}
	return t, nil
}

func (t *Trace) add(step, detail string, passed bool, format string, args ...any) {
	*t = append(*t, Condition{
		Step:   step,
		Detail: detail,
		Passed: passed,
		Reason: fmt.Sprintf(format, args...),
	})
}

// guardVerdict is what the chain decided about a matched rule.
type guardVerdict struct {
	// suppressed is set when a guardrail swallowed the event; reason is what
	// the events screen shows.
	suppressed bool
	reason     string
}

// runGuards applies the guardrails in ledger order — silence, dedup, cooldown,
// daily cap — and appends what each one decided to the trace. The order is the
// contract: a silenced host must not consume a rule's daily cap, and a
// duplicate must not reset a cooldown.
func (e *Engine) runGuards(ctx context.Context, ev Event, rule store.Rule, trace *Trace) (guardVerdict, error) {
	now := e.now()

	if verdict, err := e.guardSilence(ctx, ev, trace); err != nil || verdict.suppressed {
		return verdict, err
	}
	if verdict, err := e.guardDedup(ctx, ev, now, trace); err != nil || verdict.suppressed {
		return verdict, err
	}
	if verdict, err := e.guardCooldown(ctx, rule, now, trace); err != nil || verdict.suppressed {
		return verdict, err
	}
	return e.guardDailyCap(ctx, rule, now, trace)
}

// guardSilence swallows an event a silence covers. A global-scope silence is
// the reactive kill switch: it is one row, not a separate flag, so pausing
// everything obeys the same mandatory expiry as muting one alert.
func (e *Engine) guardSilence(ctx context.Context, ev Event, trace *Trace) (guardVerdict, error) {
	silences, err := e.store.ListSilences(ctx, true)
	if err != nil {
		return guardVerdict{}, err
	}

	for _, sil := range silences {
		if !silenceCovers(sil, ev) {
			continue
		}
		swallowed, err := e.store.CountSuppressedSince(ctx, sil.CreatedAt)
		if err != nil {
			return guardVerdict{}, err
		}
		// This event is about to become the next one swallowed, and the
		// banner counts it.
		reason := fmt.Sprintf("silenced: %s until %s (%d swallowed)",
			silenceScope(sil), clock(sil.ExpiresAt), swallowed+1)
		trace.add(StepSilence, silenceScope(sil), false, "%s", reason)
		return guardVerdict{suppressed: true, reason: reason}, nil
	}

	trace.add(StepSilence, "", true, "no silence covers this event")
	return guardVerdict{}, nil
}

// silenceCovers reports whether a silence's scope matches an event.
func silenceCovers(sil store.Silence, ev Event) bool {
	switch sil.ScopeKind {
	case store.ScopeGlobal:
		return true
	case store.ScopeAlert:
		return sil.ScopeValue == ev.Name
	case store.ScopeHost:
		return sil.ScopeValue == ev.scopeValue()
	default:
		return false
	}
}

func silenceScope(sil store.Silence) string {
	if sil.ScopeKind == store.ScopeGlobal {
		return "global pause"
	}
	return sil.ScopeKind + " " + sil.ScopeValue
}

// guardDedup swallows a repeat of an event already decided inside the window.
// The anchor is the last event with this key that was not itself swallowed, so
// a source retrying every ten seconds cannot slide the window forward forever.
func (e *Engine) guardDedup(ctx context.Context, ev Event, now time.Time, trace *Trace) (guardVerdict, error) {
	if ev.DedupKey == "" || e.dedupWindow <= 0 {
		trace.add(StepDedup, "", true, "no dedup key")
		return guardVerdict{}, nil
	}

	previous, err := e.store.LastEventByDedupKey(ctx, ev.DedupKey)
	if errors.Is(err, store.ErrNotFound) {
		trace.add(StepDedup, ev.DedupKey, true, "first event with this key")
		return guardVerdict{}, nil
	}
	if err != nil {
		return guardVerdict{}, err
	}

	age := now.Sub(previous.ReceivedAt)
	if age >= e.dedupWindow {
		trace.add(StepDedup, ev.DedupKey, true, "last seen %s ago, window %s",
			humanDuration(age), humanDuration(e.dedupWindow))
		return guardVerdict{}, nil
	}
	reason := fmt.Sprintf("duplicate of event %d within %s",
		previous.ID, humanDuration(e.dedupWindow))
	trace.add(StepDedup, ev.DedupKey, false, "%s", reason)
	return guardVerdict{suppressed: true, reason: reason}, nil
}

// guardCooldown swallows a rule that fired too recently. The ledger is
// rule_fire rows, so a crash-loop restart does not hand the rule a fresh
// budget.
func (e *Engine) guardCooldown(ctx context.Context, rule store.Rule, now time.Time, trace *Trace) (guardVerdict, error) {
	if rule.CooldownS <= 0 {
		trace.add(StepCooldown, rule.Name, true, "no cooldown")
		return guardVerdict{}, nil
	}
	cooldown := time.Duration(rule.CooldownS) * time.Second

	last, err := e.store.LastRuleFire(ctx, rule.ID)
	if errors.Is(err, store.ErrNotFound) {
		trace.add(StepCooldown, rule.Name, true, "cooldown %s, never fired", humanDuration(cooldown))
		return guardVerdict{}, nil
	}
	if err != nil {
		return guardVerdict{}, err
	}

	eligible := last.Ts.Add(cooldown)
	if !now.Before(eligible) {
		trace.add(StepCooldown, rule.Name, true, "cooldown %s, last fired %s ago",
			humanDuration(cooldown), humanDuration(now.Sub(last.Ts)))
		return guardVerdict{}, nil
	}
	reason := fmt.Sprintf("cooldown %s, next eligible %s",
		humanDuration(cooldown), clock(eligible))
	trace.add(StepCooldown, rule.Name, false, "%s", reason)
	return guardVerdict{suppressed: true, reason: reason}, nil
}

// capWindow is the span a daily cap counts over. Rolling, not calendar: a
// rule that fired ten times at 23:59 must not get ten more at 00:01.
const capWindow = 24 * time.Hour

// guardDailyCap disables a rule that has spent its budget. A cap throttling
// silently would leave a misfiring rule firing forever at the cap rate; a rule
// that disables itself is a thing the operator has to come and look at, which
// is the point (architecture/overview.md).
func (e *Engine) guardDailyCap(ctx context.Context, rule store.Rule, now time.Time, trace *Trace) (guardVerdict, error) {
	if rule.DailyCap <= 0 {
		trace.add(StepCap, rule.Name, true, "no daily cap")
		return guardVerdict{}, nil
	}

	fired, err := e.store.CountRuleFires(ctx, rule.ID, now.Add(-capWindow))
	if err != nil {
		return guardVerdict{}, err
	}
	if fired < rule.DailyCap {
		trace.add(StepCap, rule.Name, true, "daily cap %d, %d used in %s",
			rule.DailyCap, fired, humanDuration(capWindow))
		return guardVerdict{}, nil
	}

	if err := e.store.SetRuleEnabled(ctx, rule.Name, false); err != nil {
		return guardVerdict{}, err
	}
	reason := fmt.Sprintf("daily cap %d reached; rule %s disabled", rule.DailyCap, rule.Name)
	trace.add(StepCap, rule.Name, false, "%s", reason)

	e.notify(ctx, capDisabledNotification(rule))
	return guardVerdict{suppressed: true, reason: reason}, nil
}

// clock renders a wall-clock time the way a reason string carries one.
func clock(t time.Time) string { return t.UTC().Format("15:04") }

// humanDuration renders a duration without the trailing zero units
// time.Duration's own String leaves ("6h0m0s").
func humanDuration(d time.Duration) string {
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
		return d.Round(time.Second).String()
	}
}
