package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// BuildPlaybookSpec resolves a matched playbook-action rule into the run the
// queue takes. It is the daemon's reactive.PlaybookBuilder: it shapes only the
// executable half — Kind, Argv, Dir, DirReadOnly, Source — and never the
// identity fields (Runbook, Trigger, DedupKey, ConcurrencyKey, Timeout), which
// the engine stamps after the builder returns. The engine still owns enqueue,
// gate, and approval; this only answers what a match runs and from where.
//
// A rule playbook is a plain, vault-less, inventory-discovery play for v1: no
// run-secrets or vault binding on rules, and a hooked source is refused — a
// rule never composes the lifecycle workflow `exocomp run` owns. The source
// checkout must exist on disk (containedPath resolves symlinks on the deepest
// existing ancestor), the same posture as `playbook run --source`.
func (s *Service) BuildPlaybookSpec(ctx context.Context, _ reactive.Event, rule store.Rule) (queue.Spec, error) {
	if rule.Action.Source == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action names no source", rule.Name)
	}
	if rule.Action.Playbook == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action names no playbook", rule.Name)
	}
	if rule.Action.Limit == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action needs a --limit (default-deny)", rule.Name)
	}
	// The passthrough argv is discarded here; it is re-derived from the same
	// fields when the spec is assembled below. What this call buys is the
	// shape validation: a limit or tag ansible cannot read is refused before
	// the run is ever built.
	if _, _, err := AnsiblePassthrough(rule.Action.Limit,
		strings.Join(rule.Action.Tags, ","), "", rule.Action.Check, rule.Action.Diff, nil); err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}

	src, err := s.store.SourceByName(ctx, rule.Action.Source)
	if errors.Is(err, store.ErrNotFound) {
		return queue.Spec{}, fmt.Errorf("rule %q: no source %q", rule.Name, rule.Action.Source)
	}
	if err != nil {
		return queue.Spec{}, err
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: resolve source checkout: %w", rule.Name, err)
	}

	// The override-wins hooks resolution covers BOTH hook sources — the
	// checkout's own .exocomp/hooks.yml and the operator override under the
	// data dir — so a source hooked either way is refused. The override path
	// mirrors cmd/exocomp/run.go's hooksOverridePath; the two must agree.
	hk, _, err := s.ResolveSourceHooks(ctx,
		filepath.Join(s.dataDir, "hooks", rule.Action.Source+".yml"), rule.Action.Source)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}
	if !hk.Empty() {
		return queue.Spec{}, fmt.Errorf(
			"rule %q: source %q has hooks or a gate; a rule playbook is a plain run",
			rule.Name, rule.Action.Source)
	}

	if filepath.IsAbs(rule.Action.Playbook) {
		return queue.Spec{}, fmt.Errorf(
			"rule %q: playbook %q must be relative to the source checkout",
			rule.Name, rule.Action.Playbook)
	}
	abs, err := containedPath(loc.Root, rule.Action.Playbook)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}

	passthrough, _, err := AnsiblePassthrough(rule.Action.Limit,
		strings.Join(rule.Action.Tags, ","), "", rule.Action.Check, rule.Action.Diff, nil)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}
	// The inventory stays empty: a rule playbook runs against the checkout's
	// own ./ansible.cfg inventory discovery, like a bare `playbook run --source`.
	return queue.Spec{
		Kind:             executor.KindAnsible,
		Argv:             executor.AnsibleArgv(abs, "", passthrough...),
		Dir:              loc.Root,
		DirReadOnly:      loc.ReadOnly,
		Source:           rule.Action.Source,
		EgressPorts:      rule.Action.Egress,
		SSHIdentityFiles: rule.Action.SSHIdentityFiles,
	}, nil
}
