package queue

import (
	"context"
	"log/slog"
	"sync"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// storeSink writes driver events to job_event. Events are telemetry: a write
// that fails is logged, never allowed to change a job's outcome.
type storeSink struct {
	store *store.Store
	jobID int64
	log   *slog.Logger

	mu sync.Mutex
}

func newStoreSink(st *store.Store, jobID int64, log *slog.Logger) *storeSink {
	return &storeSink{store: st, jobID: jobID, log: log}
}

// Emit implements executor.EventSink.
func (s *storeSink) Emit(ev executor.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	level := ev.Level
	if level == "" {
		level = store.LevelInfo
	}
	// Not the job's context: a cancelled job must still keep the tail of its
	// own log.
	_, err := s.store.AppendJobEvent(context.Background(), store.JobEvent{
		JobID:   s.jobID,
		Ts:      ev.Ts,
		Level:   level,
		Msg:     ev.Msg,
		RawJSON: ev.RawJSON,
	})
	if err != nil {
		s.log.Error("append job event", "job", s.jobID, "err", err)
	}
}
