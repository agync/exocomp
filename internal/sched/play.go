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

// InventoryResolver resolves a standing inventory by name to its bytes on disk,
// resolved filesystem path, and whether it lives inside a source checkout
// rather than being a tofu-materialized artifact.
type InventoryResolver func(ctx context.Context, name string) (standing store.StandingInventory, resolvedPath string, sourceLinked bool, err error)

// playSpec builds the one job a play schedule fires. For tofu-materialized
// inventories (`sourceLinked == false`), the play carries no --inventory of its
// own and the daemon's ansible driver copies that file into the run's private
// dir. For source-linked static inventories (`sourceLinked == true`), the play
// passes `--inventory <resolvedPath>` directly so Ansible reads sibling
// `group_vars/` and `host_vars/` inside the source tree.
//
// It fails closed. A schedule whose inventory is unregistered, not a
// materialized/static file, gone from disk, unresolvable for its source, or
// names no hosts enqueues NOTHING and reports why: a recurring converge that
// quietly runs against an empty inventory looks green forever while converging
// nobody.
//
// A schedule that names a source runs from that source's checkout and carries
// the name on the spec, so queue admission folds in the source's default egress
// and run-secret binding (Decisions 0023, 0025). A schedule with no source is
// unchanged: the playbook is used as given and the spec names none.
func playSpec(ctx context.Context, st *store.Store, log *slog.Logger, sc store.Schedule, trigger string,
	resolve SourceResolver, resolveInv InventoryResolver) (queue.Spec, error) {
	inventory := sc.Args.Target
	if inventory == "" {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs an inventory", sc.Name, KindPlay)
	}
	if len(sc.Args.Argv) == 0 {
		return queue.Spec{}, fmt.Errorf("schedule %q: kind %s needs a playbook", sc.Name, KindPlay)
	}
	hosts, resolvedInv, sourceLinked, err := standingInventoryHosts(ctx, st, inventory, resolveInv)
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
	if sourceLinked {
		spec.Argv = executor.AnsibleArgv(playbook, resolvedInv, sc.Args.Argv[1:]...)
	} else {
		spec.Argv = executor.AnsibleArtifactArgv(playbook, sc.Args.Argv[1:]...)
	}

	attrs := []any{"schedule", sc.Name, "inventory", inventory, "hosts", hosts}
	if spec.Source != "" {
		attrs = append(attrs, "source", spec.Source)
	}
	log.Info("sched: play on standing inventory", attrs...)
	return spec, nil
}

// standingInventoryHosts returns how many hosts a standing inventory currently
// names, its resolved path, and whether it is source-linked, failing closed on
// missing/stale/hostless inventories or missing source resolvers.
func standingInventoryHosts(ctx context.Context, st *store.Store, name string,
	resolveInv InventoryResolver) (hosts int, resolvedPath string, sourceLinked bool, err error) {
	var standing store.StandingInventory
	if resolveInv != nil {
		standing, resolvedPath, sourceLinked, err = resolveInv(ctx, name)
	} else {
		var inv store.Inventory
		inv, err = st.InventoryByName(ctx, name)
		if err == nil && inv.SourceID != nil {
			return 0, "", false, fmt.Errorf("inventory %q belongs to a source that cannot be resolved here", name)
		}
		if err == nil {
			standing, err = st.StandingInventory(ctx, name)
			resolvedPath = standing.Inventory.Path
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 0, "", false, fmt.Errorf("inventory %q is not registered", name)
	case err != nil:
		return 0, "", false, err
	}
	parsed, err := executor.InventoryHosts(standing.Body)
	if err != nil {
		return 0, "", false, fmt.Errorf("inventory %q: %w", name, err)
	}
	if len(parsed) == 0 {
		return 0, "", false, fmt.Errorf("inventory %q names no hosts (refreshed %s); not running a play against nothing",
			name, standing.RefreshedAt.Format("2006-01-02 15:04:05Z"))
	}
	return len(parsed), resolvedPath, sourceLinked, nil
}
