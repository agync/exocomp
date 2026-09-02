package reactive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Explanation is a stored event with the trace that decided it.
type Explanation struct {
	Event store.Event
	Trace Trace
}

// Explain returns why one event ended the way it did. The trace is read back
// from the row rather than recomputed: the ledgers it consulted — silences,
// fires, caps — have moved on since, and a replay would answer a different
// question than the one the operator is asking.
func Explain(ctx context.Context, st *store.Store, eventID int64) (Explanation, error) {
	event, err := st.GetEvent(ctx, eventID)
	if err != nil {
		return Explanation{}, err
	}
	trace, err := DecodeTrace(event.Trace)
	if err != nil {
		return Explanation{}, err
	}
	return Explanation{Event: event, Trace: trace}, nil
}

// Render writes the explanation as the CLI prints it, reflowed to fit the
// 80-column terminal the TUI's trace view is designed against.
func (x Explanation) Render() string {
	var b strings.Builder
	e := x.Event

	fmt.Fprintf(&b, "event %d  %s  %s\n", e.ID, e.Source,
		e.ReceivedAt.Format("2006-01-02 15:04:05Z"))
	fmt.Fprintf(&b, "outcome: %s\n", e.Outcome)
	if e.MatchedRule != "" {
		fmt.Fprintf(&b, "rule:    %s\n", e.MatchedRule)
	}
	if e.DedupKey != "" {
		fmt.Fprintf(&b, "dedup:   %s\n", e.DedupKey)
	}
	if e.JobID != nil {
		fmt.Fprintf(&b, "job:     %d\n", *e.JobID)
	}
	fmt.Fprintf(&b, "reason:  %s\n", e.Reason)

	if len(x.Trace) == 0 {
		b.WriteString("\nno trace recorded\n")
	} else {
		b.WriteString("\ntrace:\n")
		for _, c := range x.Trace {
			mark := "x"
			if c.Passed {
				mark = "."
			}
			label := c.Step
			if c.Detail != "" {
				label += " " + c.Detail
			}
			fmt.Fprintf(&b, "  %s %-24s %s\n", mark, label, c.Reason)
		}
	}
	x.renderLedger(&b)
	return b.String()
}

// renderLedger appends a check transition's hysteresis ledger — the "WHY THIS
// COUNTED" observations, the transition line, the hysteresis note, and the
// cold-start body — when the event is a check event that carries one. Alert and
// non-check events carry no ledger, so their output is unchanged. The daemon's
// start time is not reachable here (the CLI reads the store, not the server),
// so the cold-start body is the unanchored form; the web card anchors it to
// whoami.server.started_at.
func (x Explanation) renderLedger(b *strings.Builder) {
	e := x.Event
	if e.Source != SourceCheck || e.CheckObservations == "" || e.CheckObservations == "[]" {
		return
	}
	var obs []CheckObservation
	if err := json.Unmarshal([]byte(e.CheckObservations), &obs); err != nil || len(obs) == 0 {
		return
	}
	b.WriteString("\nledger:\n")
	for i, o := range obs {
		at := o.ObservedAt
		if ts, err := time.Parse(time.RFC3339, o.ObservedAt); err == nil {
			at = ts.UTC().Format("15:04:05")
		}
		marker := ""
		if i == len(obs)-1 {
			marker = " → transition"
		}
		fmt.Fprintf(b, "  observed %s %s · %d of %d%s\n",
			o.Status, at, o.Consecutive, DefaultHysteresis, marker)
	}
	b.WriteString("  hysteresis N=3 · one blip is not a transition\n")
	b.WriteString("  A status must hold for 3 consecutive observations before it becomes an event. Absence of history is cold-start, not health.\n")
}
