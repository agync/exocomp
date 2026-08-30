package reactive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// WorkspaceRunbookPrefix is what a tofu job's runbook carries: it is the only
// place the workspace a queued tofu job belongs to is recorded, and the
// daemon reads it back to rebuild that job's driver. `workspace plan|apply`
// writes the same prefix; TestWorkspaceRunbookPrefixMatchesTheCLI pins it.
const WorkspaceRunbookPrefix = "workspace:"

// PlanArtifactKind labels a saved plan file, matching what `workspace plan`
// records so an approval can show the plan the apply will consume.
const PlanArtifactKind = "tofu-plan"

// Trigger is what a job this package enqueues records as its origin.
const Trigger = "event"

// PlaybookBuilder turns a matched playbook-action rule into the run the queue
// takes. The engine stays in charge: it stamps the identity fields (Runbook,
// Trigger, DedupKey, ConcurrencyKey, Timeout) and owns enqueue/gate/approval;
// the builder shapes only the executable half (Kind, Argv, Dir, DirReadOnly,
// Source). Wired by the daemon (service-backed); nil means a playbook rule can
// never fire.
type PlaybookBuilder interface {
	BuildPlaybookSpec(ctx context.Context, ev Event, rule store.Rule) (queue.Spec, error)
}

// WorkspaceRunbook is the runbook name a job against a workspace carries.
func WorkspaceRunbook(name string) string { return WorkspaceRunbookPrefix + name }

// WorkspaceOf reads the workspace back out of a runbook name.
func WorkspaceOf(runbook string) (string, bool) {
	name, ok := strings.CutPrefix(runbook, WorkspaceRunbookPrefix)
	return name, ok && name != ""
}

// Destructive reports whether an action kind changes infrastructure in a way
// running it again cannot undo. Destructive actions are approval-gated
// whatever the rule says: default-deny on destructive runbook kinds is a
// property of the queue, not a checkbox on a rule.
func Destructive(kind string) bool {
	return kind == store.ActionTofuApply
}

// planRef is what an approval records about the plan it is deciding on.
type planRef struct {
	artifact  *int64
	workspace string
	serial    int64
}

// PlanSpec is the queued job a drift check runs. The driver adds
// -detailed-exitcode, so "changes present" arrives as a plan outcome event
// rather than as a failed job. The scheduler's tofu-plan kind and a rule's
// tofu-plan action produce the same spec, differing only in the trigger.
func PlanSpec(ws store.Workspace, trigger string) queue.Spec {
	return queue.Spec{
		Runbook:        WorkspaceRunbook(ws.Name),
		Kind:           executor.KindTofu,
		Trigger:        trigger,
		Argv:           executor.TofuArgv("plan"),
		Dir:            ws.TofuDir,
		ConcurrencyKey: "tofu:" + ws.Name,
	}
}

// buildSpec turns a matched rule's action into the job spec the queue takes,
// and the plan reference an approval for it would carry.
func (e *Engine) buildSpec(ctx context.Context, ev Event, rule store.Rule) (queue.Spec, planRef, error) {
	switch rule.ActionKind {
	case store.ActionShell:
		if len(rule.Action.Argv) == 0 {
			return queue.Spec{}, planRef{}, fmt.Errorf("rule %q: action %s has no argv",
				rule.Name, rule.ActionKind)
		}
		return queue.Spec{
			Runbook:        "rule:" + rule.Name,
			Kind:           store.ActionShell,
			Trigger:        Trigger,
			Argv:           rule.Action.Argv,
			ConcurrencyKey: "rule:" + rule.Name,
			DedupKey:       ev.DedupKey,
			Timeout:        time.Duration(rule.Action.Timeout),
		}, planRef{}, nil

	case store.ActionTofuPlan:
		ws, err := e.workspace(ctx, rule)
		if err != nil {
			return queue.Spec{}, planRef{}, err
		}
		spec := PlanSpec(ws, Trigger)
		spec.DedupKey = ev.DedupKey
		return spec, planRef{workspace: ws.Name, serial: ws.Serial}, nil

	case store.ActionTofuApply:
		ws, err := e.workspace(ctx, rule)
		if err != nil {
			return queue.Spec{}, planRef{}, err
		}
		saved, err := e.latestPlanArtifact(ctx, ws.Name)
		if err != nil {
			return queue.Spec{}, planRef{}, err
		}

		argv := executor.TofuArgv("apply", "-auto-approve")
		ref := planRef{workspace: ws.Name, serial: ws.Serial}
		if saved != nil {
			argv = executor.TofuArgv("apply", saved.Path)
			id := saved.ID
			ref.artifact = &id
		}
		return queue.Spec{
			Runbook:        WorkspaceRunbook(ws.Name),
			Kind:           executor.KindTofu,
			Trigger:        Trigger,
			Argv:           argv,
			Dir:            ws.TofuDir,
			ConcurrencyKey: "tofu:" + ws.Name,
			DedupKey:       ev.DedupKey,
			Timeout:        time.Duration(rule.Action.Timeout),
		}, ref, nil

	case store.ActionPlaybook:
		if e.playbooks == nil {
			return queue.Spec{}, planRef{}, fmt.Errorf(
				"rule %q: playbook action has no builder wired", rule.Name)
		}
		spec, err := e.playbooks.BuildPlaybookSpec(ctx, ev, rule)
		if err != nil {
			return queue.Spec{}, planRef{}, err
		}
		// The builder shapes only the executable half; the engine stamps the
		// identity fields, the same way the shell case assembles its spec
		// (action.go:76-84).
		spec.Runbook = "rule:" + rule.Name
		spec.Trigger = Trigger
		spec.DedupKey = ev.DedupKey
		spec.ConcurrencyKey = "rule:" + rule.Name
		spec.Timeout = time.Duration(rule.Action.Timeout)
		return spec, planRef{}, nil

	default:
		return queue.Spec{}, planRef{}, fmt.Errorf("rule %q: unknown action kind %q",
			rule.Name, rule.ActionKind)
	}
}

func (e *Engine) workspace(ctx context.Context, rule store.Rule) (store.Workspace, error) {
	if rule.Action.Workspace == "" {
		return store.Workspace{}, fmt.Errorf("rule %q: action %s names no workspace",
			rule.Name, rule.ActionKind)
	}
	ws, err := e.store.WorkspaceByName(ctx, rule.Action.Workspace)
	if errors.Is(err, store.ErrNotFound) {
		return store.Workspace{}, fmt.Errorf("rule %q: no workspace %q",
			rule.Name, rule.Action.Workspace)
	}
	if err != nil {
		return store.Workspace{}, err
	}
	return ws, nil
}

// latestPlanArtifact returns the newest saved plan for a workspace, or nil
// when none was ever saved. An apply with no saved plan is a fresh apply, and
// the approval says so by carrying no artifact.
func (e *Engine) latestPlanArtifact(ctx context.Context, workspace string) (*store.Artifact, error) {
	jobs, err := e.store.ListJobs(ctx, store.JobFilter{Runbook: WorkspaceRunbook(workspace)})
	if err != nil {
		return nil, err
	}

	var newest *store.Artifact
	for _, job := range jobs {
		artifacts, err := e.store.ListArtifacts(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range artifacts {
			if a.Kind != PlanArtifactKind {
				continue
			}
			if newest == nil || a.ID > newest.ID {
				kept := a
				newest = &kept
			}
		}
	}
	return newest, nil
}
