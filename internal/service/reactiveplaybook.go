package service

import (
	"context"
	"fmt"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// BuildPlaybookSpec resolves a matched playbook-action rule into the run the
// queue takes. It is the daemon's reactive.PlaybookBuilder: it shapes only the
// executable half — Kind, Argv, Dir, DirReadOnly, Source — and never the
// identity fields (Runbook, Trigger, DedupKey, ConcurrencyKey, Timeout), which
// the engine stamps after the builder returns. The engine still owns enqueue,
// gate, and approval; this only answers what a match runs and from where.
//
// A rule playbook is an inventory-discovery play: a rule declares no run-secrets
// binding or vault project of its own, so the spec leaves the binding empty and
// queue admission fills in the source's default (Decision 0024). A hooked source
// is refused — a rule never composes the lifecycle workflow `exocomp run` owns.
// The source and playbook resolve through ResolvePlainSourcePlay, the same
// posture as `playbook run --source`.
func (s *Service) BuildPlaybookSpec(ctx context.Context, ev reactive.Event, rule store.Rule) (queue.Spec, error) {
	if rule.Action.Source == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action names no source", rule.Name)
	}
	if rule.Action.Playbook == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action names no playbook", rule.Name)
	}
	limit := rule.Action.Limit
	if strings.Contains(limit, "{{host}}") {
		if ev.Source != reactive.SourceHost {
			return queue.Spec{}, fmt.Errorf("rule %q: {{host}} templates are only valid for host-requested runs", rule.Name)
		}
		if strings.TrimSpace(ev.Host) == "" {
			return queue.Spec{}, fmt.Errorf("rule %q: host-requested run has no requesting host", rule.Name)
		}
		limit = strings.ReplaceAll(limit, "{{host}}", ev.Host)
	}
	if limit == "" {
		return queue.Spec{}, fmt.Errorf("rule %q: playbook action needs a --limit (default-deny)", rule.Name)
	}
	if ev.Source == reactive.SourceHost && !rule.Action.AllowCrossHost {
		if limit != ev.Host {
			return queue.Spec{}, fmt.Errorf("rule %q: host request may only target the requesting host (%s)", rule.Name, ev.Host)
		}
	}
	// The passthrough argv is discarded here; it is re-derived from the same
	// fields when the spec is assembled below. What this call buys is the
	// shape validation: a limit or tag ansible cannot read is refused before
	// the run is ever built.
	if _, _, err := AnsiblePassthrough(limit,
		strings.Join(rule.Action.Tags, ","), "", rule.Action.Check, rule.Action.Diff, nil); err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}

	abs, root, readOnly, err := s.ResolvePlainSourcePlay(ctx, rule.Action.Source, rule.Action.Playbook)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}

	passthrough, _, err := AnsiblePassthrough(limit,
		strings.Join(rule.Action.Tags, ","), "", rule.Action.Check, rule.Action.Diff, nil)
	if err != nil {
		return queue.Spec{}, fmt.Errorf("rule %q: %w", rule.Name, err)
	}
	// The inventory stays empty: a rule playbook runs against the checkout's
	// own ./ansible.cfg inventory discovery, like a bare `playbook run --source`.
	return queue.Spec{
		Kind:             executor.KindAnsible,
		Argv:             executor.AnsibleArgv(abs, "", passthrough...),
		Dir:              root,
		DirReadOnly:      readOnly,
		Source:           rule.Action.Source,
		EgressPorts:      rule.Action.Egress,
		SSHIdentityFiles: rule.Action.SSHIdentityFiles,
	}, nil
}
