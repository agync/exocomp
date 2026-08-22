package reactive

import (
	"context"
	"fmt"
	"strings"

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
		return b.String()
	}
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
	return b.String()
}
