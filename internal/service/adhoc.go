package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// AdhocInput is a resolved ad-hoc ansible run: `ansible <pattern> -i <inv> -m
// <module> -a "<args>"`, the shape `exocomp command`/`exocomp ansible` and the
// browser's run-a-command form both assemble into a queue.Spec. Dir is the
// working directory a --source run resolves to (empty runs in the caller's own
// directory); InventoryPath/InventoryName are the already-resolved pair
// Service.ResolveInventory returns.
type AdhocInput struct {
	// Runbook labels the job so `command` and `ansible` stay tellable apart in
	// the job list; empty defaults to "command".
	Runbook string
	Pattern string
	// Module defaults to "command" (no shell) when empty; "shell" switches to
	// the shell module so a pipe or redirect works.
	Module              string
	Args                string
	Become              bool
	Dir                 string
	InventoryPath       string
	InventoryName       string
	VaultProject        string
	RunSecrets          string
	RunnerName          string
	RequireApproval     bool
	TolerateUnreachable bool
	IdempotencyKey      string
	Timeout             time.Duration
	// Actor identifies a non-CLI caller (an API/web token) for the job's event
	// ledger; the zero value records nothing (see recordActor).
	Actor Actor
}

// AdhocResult is either a gated run (Gated set, Approval populated, Run nil) or
// a plain run (Run populated) — the ad-hoc mirror of ApplyResult.
type AdhocResult struct {
	Gated    bool
	Approval GatedApproval
	Run      *RunResult
}

// adhocModuleName is the ansible module-name shape: a leading letter/digit/
// underscore, then letters, digits, underscores and dots (so
// `ansible.builtin.ping` is admitted). Anything else — a leading dash, a
// space, a shell metacharacter — is refused, so a module field cannot smuggle
// a flag or a second token onto the argv.
var adhocModuleName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.]*$`)

// ValidHostPattern guards an ansible host pattern: it must be non-empty and
// may not start with a dash (read as a flag) or carry a control character
// (split or truncate the single argv token it becomes).
func ValidHostPattern(pattern string) error {
	if pattern == "" {
		return errors.New("service: adhoc: host pattern is required")
	}
	if strings.HasPrefix(pattern, "-") {
		return fmt.Errorf("service: adhoc: host pattern %q starts with a dash", pattern)
	}
	if hasControl(pattern) {
		return errors.New("service: adhoc: host pattern contains a control character")
	}
	return nil
}

// ValidModuleName guards an ansible module name against the plain module-name
// shape.
func ValidModuleName(module string) error {
	if !adhocModuleName.MatchString(module) {
		return fmt.Errorf("service: adhoc: module %q is not a valid module name", module)
	}
	return nil
}

// ValidModuleArgs guards an ansible module's -a string: it may not start with
// a dash (read as a flag) or carry a control character.
func ValidModuleArgs(args string) error {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("service: adhoc: module args %q start with a dash", args)
	}
	if hasControl(args) {
		return errors.New("service: adhoc: module args contain a control character")
	}
	return nil
}

// hasControl reports whether s carries a control character (NUL, a newline,
// or any other), the bytes a single argv token cannot safely carry.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) }) >= 0
}

// RunAdhoc assembles the ad-hoc ansible job spec and enqueues it, gating
// through the approval gate instead when RequireApproval asks for it — the
// same require-approval seam ApplyWorkspace's gatedApply uses. The pattern,
// module, and args are guarded here (not left to the caller) so every face —
// the CLI's own flag parser and the browser's plain form fields alike — gets
// the same injection safety.
func (s *Service) RunAdhoc(ctx context.Context, enq Enqueuer, deriver reactive.Deriver, in AdhocInput) (AdhocResult, error) {
	if err := ValidHostPattern(in.Pattern); err != nil {
		return AdhocResult{}, err
	}
	module := in.Module
	if module == "" {
		module = "command"
	}
	if err := ValidModuleName(module); err != nil {
		return AdhocResult{}, err
	}
	if err := ValidModuleArgs(in.Args); err != nil {
		return AdhocResult{}, err
	}
	runbook := in.Runbook
	if runbook == "" {
		runbook = "command"
	}

	var passthrough []string
	if in.Become {
		passthrough = append(passthrough, "--become")
	}

	spec := queue.Spec{
		Runbook:             runbook,
		Kind:                executor.KindAnsible,
		Trigger:             TriggerManual,
		Argv:                executor.AnsibleAdhocArgv(in.Pattern, in.InventoryPath, module, in.Args, passthrough...),
		Dir:                 in.Dir,
		VaultProject:        in.VaultProject,
		RunSecretBinding:    in.RunSecrets,
		Inventory:           in.InventoryName,
		TolerateUnreachable: in.TolerateUnreachable,
		RunnerName:          in.RunnerName,
		IdempotencyKey:      in.IdempotencyKey,
		Timeout:             in.Timeout,
	}

	if in.RequireApproval {
		return s.gatedAdhoc(ctx, enq, deriver, spec, in.Actor)
	}

	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		return AdhocResult{}, err
	}
	if err := s.recordActor(ctx, job.ID, in.Actor); err != nil {
		return AdhocResult{}, err
	}
	return AdhocResult{Run: &RunResult{Job: job}}, nil
}

// gatedAdhoc routes a --require-approval ad-hoc job through the same gate and
// approval machinery ApplyWorkspace's gatedApply uses: EnqueueGated lands the
// job in awaiting_approval where no worker can claim it, and a fresh approval
// is created and pointed at it. A retry under the same idempotency key reuses
// the job's existing approval rather than colliding on the table's job_id
// UNIQUE constraint.
func (s *Service) gatedAdhoc(ctx context.Context, enq Enqueuer, deriver reactive.Deriver,
	spec queue.Spec, actor Actor) (AdhocResult, error) {
	job, err := enq.EnqueueGated(ctx, spec)
	if err != nil {
		return AdhocResult{}, err
	}
	if err := s.recordActor(ctx, job.ID, actor); err != nil {
		return AdhocResult{}, err
	}

	if existing, aerr := s.store.ApprovalByJob(ctx, job.ID); aerr == nil {
		return AdhocResult{Gated: true, Approval: GatedApproval{
			ApprovalID: existing.ID, JobID: existing.JobID,
			State: existing.State, ExpiresAt: existing.ExpiresAt,
		}}, nil
	} else if !errors.Is(aerr, store.ErrNotFound) {
		return AdhocResult{}, aerr
	}

	_, hash, err := reactive.ApprovalToken(deriver, job.ID)
	if err != nil {
		return AdhocResult{}, err
	}
	now := s.now().UTC()
	approval, err := s.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, RequestedAt: now, ExpiresAt: now.Add(reactive.DefaultApprovalTTL), TokenHash: hash,
	})
	if err != nil {
		return AdhocResult{}, err
	}
	return AdhocResult{Gated: true, Approval: GatedApproval{
		ApprovalID: approval.ID, JobID: job.ID, State: approval.State, ExpiresAt: approval.ExpiresAt,
	}}, nil
}
