package sched

import (
	"context"
	"errors"
	"fmt"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// KindPlay is a recurring ansible play over a STANDING inventory: a registered
// inventory the schedule names, rather than a host list frozen into the row.
// Paired with the `tf:<workspace>` inventory a handoff apply refreshes, it is
// what makes a freshly provisioned host join the recurring converge without the
// schedule being touched — the next fire reads whatever the last apply
// materialized.
//
// The kind is defined here rather than in the store because the store keeps a
// schedule's kind as an opaque string; what a kind MEANS is this package's.
const KindPlay = "play"

// playSpec builds the one job a play schedule fires. The play carries no
// --inventory of its own: Spec.Inventory names the registered one, and the
// daemon's ansible driver copies that file into the run's own 0700 private
// dir, so a scheduled converge reaches the tofu-derived hosts under the same
// boundary the handoff's play does.
//
// It fails closed. A schedule whose inventory is unregistered, not a
// materialized file, gone from disk, or names no hosts enqueues NOTHING and
// reports why: a recurring converge that quietly runs against an empty
// inventory looks green forever while converging nobody.
func (s *Scheduler) playSpec(ctx context.Context, sc store.Schedule) (queue.Spec, error) {
	inventory := sc.Args.Target
	if inventory == "" {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs an inventory", sc.Name, KindPlay)
	}
	if len(sc.Args.Argv) == 0 {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs a playbook", sc.Name, KindPlay)
	}
	hosts, err := s.inventoryHosts(ctx, inventory)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("schedule %q: %w", sc.Name, err)
	}

	s.log.Info("sched: play on standing inventory",
		"schedule", sc.Name, "inventory", inventory, "hosts", hosts)
	return queue.Spec{
		Runbook:             sc.Name,
		Kind:                executor.KindAnsible,
		Trigger:             Trigger,
		Argv:                executor.AnsibleArtifactArgv(sc.Args.Argv[0], sc.Args.Argv[1:]...),
		Inventory:           inventory,
		ConcurrencyKey:      sc.ConcurrencyKey,
		TolerateUnreachable: sc.Args.TolerateUnreachable,
	}, nil
}

// inventoryHosts returns how many hosts a standing inventory currently names,
// erroring when it names none. The count is read from the same bytes the run
// will consume, so what gates the fire is what the play would have targeted.
func (s *Scheduler) inventoryHosts(ctx context.Context, name string) (int, error) {
	standing, err := s.store.StandingInventory(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 0, fmt.Errorf("inventory %q is not registered", name)
	case err != nil:
		return 0, err
	}
	hosts, err := executor.InventoryHosts(standing.Body)
	if err != nil {
		return 0, fmt.Errorf("inventory %q: %w", name, err)
	}
	if len(hosts) == 0 {
		return 0, fmt.Errorf("inventory %q names no hosts (refreshed %s); not running a play against nothing",
			name, standing.RefreshedAt.Format("2006-01-02 15:04:05Z"))
	}
	return len(hosts), nil
}
