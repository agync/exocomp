package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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

// SourceResolver turns a play schedule's source and playbook into what the run
// executes from: the playbook confined to the source checkout, the checkout
// root as the working directory, and the source's read-only posture. The daemon
// supplies service.Service.ResolvePlainSourcePlay, the resolution a rule-fired
// playbook uses, so a scheduled play and a rule play see the same tree. It is
// nil only where no data directory is known; a schedule that names a source
// then fails closed.
type SourceResolver func(ctx context.Context, source, playbook string) (abs, dir string, readOnly bool, err error)

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
//
// A schedule that names a source runs from that source's checkout and carries
// the name on the spec, so queue admission folds in the source's default egress
// and run-secret binding (Decisions 0023, 0025). A schedule with no source is
// unchanged: the playbook is used as given and the spec names none.
func playSpec(ctx context.Context, st *store.Store, log *slog.Logger, sc store.Schedule, trigger string,
	resolve SourceResolver) (queue.Spec, error) {
	inventory := sc.Args.Target
	if inventory == "" {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs an inventory", sc.Name, KindPlay)
	}
	if len(sc.Args.Argv) == 0 {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs a playbook", sc.Name, KindPlay)
	}
	hosts, err := inventoryHosts(ctx, st, inventory)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("schedule %q: %w", sc.Name, err)
	}

	playbook := sc.Args.Argv[0]
	spec := queue.Spec{
		Runbook:             sc.Name,
		Kind:                executor.KindAnsible,
		Trigger:             trigger,
		Inventory:           inventory,
		ConcurrencyKey:      sc.ConcurrencyKey,
		TolerateUnreachable: sc.Args.TolerateUnreachable,
	}
	if name := sc.Args.Source; name != "" {
		if resolve == nil {
			return queue.Spec{}, fmt.Errorf("schedule %q: source %q cannot be resolved here", sc.Name, name)
		}
		abs, dir, readOnly, err := resolve(ctx, name, playbook)
		if err != nil {
			return queue.Spec{}, fmt.Errorf("schedule %q: %w", sc.Name, err)
		}
		playbook, spec.Dir, spec.DirReadOnly, spec.Source = abs, dir, readOnly, name
	}
	spec.Argv = executor.AnsibleArtifactArgv(playbook, sc.Args.Argv[1:]...)

	attrs := []any{"schedule", sc.Name, "inventory", inventory, "hosts", hosts}
	if spec.Source != "" {
		attrs = append(attrs, "source", spec.Source)
	}
	log.Info("sched: play on standing inventory", attrs...)
	return spec, nil
}

// inventoryHosts returns how many hosts a standing inventory currently names,
// erroring when it names none. The count is read from the same bytes the run
// will consume, so what gates the fire is what the play would have targeted.
func inventoryHosts(ctx context.Context, st *store.Store, name string) (int, error) {
	standing, err := st.StandingInventory(ctx, name)
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
