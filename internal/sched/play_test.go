package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// materializedInventory registers a `tf:<name>` standing inventory holding
// body, the way a successful apply's capture does.
func materializedInventory(t *testing.T, st *store.Store, name, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	if _, err := st.MaterializeInventory(t.Context(), "tf:"+name, path); err != nil {
		t.Fatalf("materialize inventory: %v", err)
	}
}

func playSchedule(t *testing.T, st *store.Store, name, inventory string) store.Schedule {
	t.Helper()
	sc, err := st.CreateSchedule(t.Context(), store.NewSchedule{
		Name:     name,
		CronExpr: "@every 1h",
		Kind:     KindPlay,
		Args:     store.ScheduleArgs{Target: inventory, Argv: []string{"/srv/play/converge.yml", "--check"}},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return sc
}

// A play schedule fires an ordinary ansible job that names the standing
// inventory rather than baking a host list into the row — which is what lets a
// host the last apply added join the converge without touching the schedule.
func TestPlayScheduleTargetsTheStandingInventory(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", `{"all": {"hosts": {"edge1": {"ansible_host": "10.0.0.1"}}}}`)
	sc := playSchedule(t, st, "converge", "tf:edge")

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	specs, err := s.buildSpecs(context.Background(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Kind != executor.KindAnsible {
		t.Errorf("kind = %q, want %q", spec.Kind, executor.KindAnsible)
	}
	if spec.Inventory != "tf:edge" {
		t.Errorf("inventory = %q, want tf:edge", spec.Inventory)
	}
	if spec.Trigger != Trigger {
		t.Errorf("trigger = %q, want %q", spec.Trigger, Trigger)
	}
	// The argv carries no --inventory: the driver copies the materialized file
	// into the run's own private dir (story 5), which is the only inventory the
	// play sees.
	joined := strings.Join(spec.Argv, " ")
	if strings.Contains(joined, "--inventory") || strings.Contains(joined, " -i ") {
		t.Errorf("argv names an inventory itself: %q", joined)
	}
	if !strings.Contains(joined, "/srv/play/converge.yml") || !strings.Contains(joined, "--check") {
		t.Errorf("argv = %q, want the playbook and its passthrough", joined)
	}
}

// The fail-closed rule: an inventory that names no hosts enqueues nothing. A
// recurring converge that runs against an empty host list reports success
// forever while converging nobody.
func TestPlayScheduleRefusesAnInventoryWithNoHosts(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", `{"all": {"vars": {"region": "eu"}}}`)
	sc := playSchedule(t, st, "converge", "tf:edge")

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	_, err := s.buildSpecs(context.Background(), sc)
	if err == nil {
		t.Fatal("buildSpecs accepted an inventory with no hosts")
	}
	if !strings.Contains(err.Error(), "names no hosts") {
		t.Errorf("err = %v, want it to say the inventory names no hosts", err)
	}
}

// A schedule whose inventory was never registered, or whose file the last apply
// never wrote, fails the same way rather than firing.
func TestPlayScheduleRefusesAMissingOrStaleInventory(t *testing.T) {
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	sc := playSchedule(t, st, "converge", "tf:never-applied")
	if _, err := s.buildSpecs(context.Background(), sc); err == nil {
		t.Fatal("buildSpecs accepted an unregistered inventory")
	}

	if _, err := st.MaterializeInventory(t.Context(), "tf:gone",
		filepath.Join(t.TempDir(), "gone.json")); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	stale := playSchedule(t, st, "converge-stale", "tf:gone")
	if _, err := s.buildSpecs(context.Background(), stale); err == nil {
		t.Fatal("buildSpecs accepted an inventory whose file is gone")
	}
}

// A fire whose spec cannot be built enqueues nothing at all: the fail-closed
// path has to stop at the queue, not merely log.
func TestPlayScheduleFireEnqueuesNothingWhenClosed(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", `{"all": {"vars": {}}}`)
	sc := playSchedule(t, st, "converge", "tf:edge")

	q := &fakeEnqueuer{store: st}
	s := New(st, q, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	s.fire(sc)

	if specs := q.Specs(); len(specs) != 0 {
		t.Fatalf("fire enqueued %d spec(s) against an empty inventory", len(specs))
	}
}

func TestPlayScheduleNeedsAnInventoryAndAPlaybook(t *testing.T) {
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	noInv := store.Schedule{Name: "a", Kind: KindPlay, Args: store.ScheduleArgs{Argv: []string{"p.yml"}}}
	if _, err := s.buildSpecs(context.Background(), noInv); err == nil {
		t.Error("buildSpecs accepted a play schedule with no inventory")
	}
	noPlay := store.Schedule{Name: "b", Kind: KindPlay, Args: store.ScheduleArgs{Target: "tf:edge"}}
	if _, err := s.buildSpecs(context.Background(), noPlay); err == nil {
		t.Error("buildSpecs accepted a play schedule with no playbook")
	}
}
