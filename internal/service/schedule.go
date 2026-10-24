package service

import (
	"context"
	"errors"
	"fmt"

	"exocomp.dev/exocomp/internal/store"
)

// invalid wraps a refusal of the caller's input as store.ErrValidation, so every
// face reports it as a bad argument.
func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", store.ErrValidation, fmt.Sprintf(format, a...))
}

// sourcePlayRefusal keeps a missing source a not-found and makes every other
// reason a source play cannot run (hooks, an escaping path) the caller's bad
// input.
func sourcePlayRefusal(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrValidation) {
		return err
	}
	return invalid("%v", err)
}

// CreateSchedule records a schedule sched.CheckNewSchedule has passed. A play
// from a source must resolve to a plain playbook in that checkout, so a
// schedule that could never fire is refused here rather than logged at every
// fire. The CLI and POST /api/v1/schedules call both, in that order. The row
// records actor as its creator: a shell schedule runs its argv as the daemon's
// user with no kernel sandbox.
func (s *Service) CreateSchedule(ctx context.Context, n store.NewSchedule, actor Actor) (store.Schedule, error) {
	if n.Args.Source != "" && len(n.Args.Argv) > 0 {
		if _, _, _, err := s.ResolvePlainSourcePlay(ctx, n.Args.Source, n.Args.Argv[0]); err != nil {
			return store.Schedule{}, sourcePlayRefusal(err)
		}
	}
	n.CreatedBy = ActorLabel(actor)
	return s.store.CreateSchedule(ctx, n)
}
