package store_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestScheduleDrillKindValidates pins that the schedule table's kind CHECK
// admits 'drill' (0019_schedule_drill.sql widened it), the same way every
// other schedule kind is proven insertable.
func TestScheduleDrillKindValidates(t *testing.T) {
	t.Parallel()
	s, _ := open(t)

	created, err := s.CreateSchedule(context.Background(), store.NewSchedule{
		Name:     "monthly-drill",
		CronExpr: "@every 720h",
		Kind:     store.ScheduleDrill,
		Args:     store.ScheduleArgs{Target: "nas"},
	})
	if err != nil {
		t.Fatalf("create drill schedule: %v", err)
	}
	if created.Kind != store.ScheduleDrill {
		t.Fatalf("kind = %q, want %q", created.Kind, store.ScheduleDrill)
	}
}
