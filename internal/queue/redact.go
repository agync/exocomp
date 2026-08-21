package queue

import (
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
)

// Option adjusts a Queue at construction.
type Option func(*Queue)

// WithRedactor scrubs known secret values out of every event on its way to
// the store. job_event is append-only and durable, so redaction has to happen
// before the write, not at read time (AGENTS.md invariant 4). A nil redactor
// leaves the queue unchanged.
func WithRedactor(r *secrets.Redactor) Option {
	return func(q *Queue) {
		if r == nil {
			return
		}
		q.wrapSink = func(next executor.EventSink) executor.EventSink {
			return redactingSink{redactor: r, next: next}
		}
	}
}

type redactingSink struct {
	redactor *secrets.Redactor
	next     executor.EventSink
}

// Emit implements executor.EventSink.
func (s redactingSink) Emit(ev executor.Event) {
	ev.Msg = string(s.redactor.Redact([]byte(ev.Msg)))
	if ev.RawJSON != "" {
		ev.RawJSON = string(s.redactor.Redact([]byte(ev.RawJSON)))
	}
	s.next.Emit(ev)
}
