package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

// This file is the client-mode WRITE seam, the mirror of remote.go's read seam.
// With a remote selected, `playbook run`, `workspace plan|apply`, `approval
// decide` and `jobs follow` POST through internal/client instead of touching a
// local data dir, and render the SAME text the local paths print (or, under
// --output json, re-emit the daemon's own write envelope byte for byte).
//
// The follow default is deliberate: a remote `playbook run` FOLLOWS the job to a
// terminal state and exits with the job's mapped code, because that is what the
// local run does and a command whose exit code changed meaning depending on
// which box ran it would be a trap for every script that already calls it.
// --no-follow opts out and prints the job id, which is the enqueue-and-walk-away
// shape an agent wants. `workspace plan|apply` do NOT follow: an apply may come
// back gated (there is no job to watch yet), and a plan's local exit-2-on-changes
// signal is not reconstructible from the read API, so a silently different exit
// code would be worse than none — those print the id and hand off to
// `jobs follow`.

// remoteJobExit maps a terminal job onto this process's exit code, the same
// mapping reportJobOutcome applies locally: a degraded success is a success.
func remoteJobExit(j wire.JobView) int {
	switch {
	case j.State == "succeeded":
		return 0
	case j.State == "degraded":
		return 0
	case j.ExitCode != nil && *j.ExitCode != 0:
		return *j.ExitCode
	default:
		return 1
	}
}

// remoteJobDuration renders a job's run time from the wire timestamps, matching
// jobDuration's local output.
func remoteJobDuration(j wire.JobView) string {
	if j.StartedAt == nil || j.FinishedAt == nil {
		return "-"
	}
	start, err := time.Parse(time.RFC3339, *j.StartedAt)
	if err != nil {
		return "-"
	}
	end, err := time.Parse(time.RFC3339, *j.FinishedAt)
	if err != nil {
		return "-"
	}
	return end.Sub(start).Round(time.Millisecond).String()
}

// renderRemoteEnqueued reports an enqueued job. The text says where the job
// went, because with --remote the queue that took it is not this box's.
func renderRemoteEnqueued(cmd *cobra.Command, mode string, job client.JobEnqueued) error {
	if mode == outputJSON {
		return emitJSON(cmd, client.KindJobEnqueued, job)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "job %d queued for the remote daemon\n", job.JobID)
	return err
}

// renderRemotePending reports a gated apply. It prints the TTL alongside the
// deadline so the operator reads the fail-closed auto-reject as intended, and
// names the decide command rather than a decision link: a link is a single-use
// capability derived from the daemon's envelope, which this box does not have.
func renderRemotePending(cmd *cobra.Command, mode string, p client.ApprovalPending) error {
	if mode == outputJSON {
		return emitJSON(cmd, client.KindApprovalPending, p)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "plan %d requires approval by policy %q\n", p.PlanArtifactID, p.Policy)
	_, _ = fmt.Fprintf(out, "job %d gated (%s); approval %d expires %s (in %s)\n",
		p.JobID, p.State, p.ApprovalID, fmtWireTS(p.ExpiresAt), ttlLabel(p.TTLSeconds))
	_, err := fmt.Fprintf(out, "decide it with `exocomp %s approval decide %d --approve`\n",
		remoteSelectorArg(cmd), p.ApprovalID)
	return err
}

func ttlLabel(seconds int64) string {
	if seconds <= 0 {
		return "expired"
	}
	return (time.Duration(seconds) * time.Second).String()
}

// remoteSelectorArg renders the --remote argument to repeat in a suggested
// command, so a printed next step targets the same daemon this one did.
func remoteSelectorArg(cmd *cobra.Command) string {
	selector, active := selectedRemote(cmd)
	if !active {
		return ""
	}
	if selector == "" {
		return "--remote="
	}
	return "--remote " + selector
}

// followRemoteJob streams the job's events to the terminal state and returns the
// exit code the shell should see. Text mode prints each event as the local tail
// does; JSON mode stays silent and emits ONE final job envelope, so a piped
// caller parses a single document rather than a stream it did not ask for.
func followRemoteJob(cmd *cobra.Command, rc *client.Client, mode string, jobID int64) (int, error) {
	out := cmd.OutOrStdout()
	emit := func(e client.Event) error {
		_, err := fmt.Fprintf(out, "[%s] %s\n", e.Level, e.Msg)
		return err
	}
	if mode == outputJSON {
		emit = func(client.Event) error { return nil }
	}
	if err := rc.FollowJob(cmd.Context(), jobID, 0, emit); err != nil {
		return 0, err
	}

	final, err := rc.GetJob(cmd.Context(), jobID)
	if err != nil {
		return 0, err
	}
	if mode == outputJSON {
		if err := emitJSON(cmd, wire.KindJob, final); err != nil {
			return 0, err
		}
		return remoteJobExit(final), nil
	}
	state := final.State
	if final.Partial {
		state += " (degraded)"
	}
	if _, err := fmt.Fprintf(out, "job %d  state: %s  duration: %s\n",
		final.ID, state, remoteJobDuration(final)); err != nil {
		return 0, err
	}
	return remoteJobExit(final), nil
}

// renderRemoteDecision reports a settled decision, keeping the local command's
// fail-closed rule: an outcome that is neither approved nor rejected settled
// nothing this call asked for, so it exits nonzero instead of reading as a
// fresh decision.
func renderRemoteDecision(cmd *cobra.Command, mode string, d client.Decision) error {
	switch d.Outcome {
	case "approved", "rejected":
		if mode == outputJSON {
			return emitJSON(cmd, client.KindDecision, d)
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), d.Message)
		return err
	default:
		err := fmt.Errorf("approval decide: %s", d.Message)
		if mode == outputJSON {
			cmd.SilenceErrors = true
			_ = wire.WriteErrorValue(cmd.OutOrStdout(), wire.NewError(wire.CodeConflict, d.Message))
			return err
		}
		return err
	}
}

// --- per-verb remote branches -----------------------------------------------

// remotePlaybookOp is `playbook run`'s inputs as they cross the wire. The
// ansible flags travel as fields, not as argv: the daemon runs the SAME
// service.AnsiblePassthrough split a local run does, so an operator's flags
// become the same argv whichever face asked.
type remotePlaybookOp struct {
	playbook            string
	inventory           string
	vaultProject        string
	runSecrets          string
	source              string
	limit               string
	tags                string
	skipTags            string
	extraVars           []string
	check               bool
	diff                bool
	tolerateUnreachable bool
	wait                bool
	noFollow            bool
	timeout             time.Duration
	idempotencyKey      string
	runnerName          string
	// egressPorts are the validated outbound TCP ports the daemon should grant
	// the run on top of the ansible kind's SSH port 22.
	egressPorts []int
}

// runRemotePlaybook enqueues a playbook on the remote daemon and, unless
// --no-follow, watches it to a terminal state. It returns the exit code the
// shell should see.
func runRemotePlaybook(cmd *cobra.Command, rc *client.Client, op remotePlaybookOp) (int, error) {
	mode, err := outputMode(cmd)
	if err != nil {
		return 0, err
	}
	if err := remoteRunGuards(op.playbook, op.inventory, op.extraVars, op.source, op.wait); err != nil {
		return 0, err
	}

	req := client.PlaybookRequest{
		Playbook:            op.playbook,
		Inventory:           op.inventory,
		VaultProject:        op.vaultProject,
		RunSecrets:          op.runSecrets,
		Source:              op.source,
		Limit:               op.limit,
		Tags:                op.tags,
		SkipTags:            op.skipTags,
		Check:               op.check,
		Diff:                op.diff,
		ExtraVars:           op.extraVars,
		TolerateUnreachable: op.tolerateUnreachable,
		IdempotencyKey:      op.idempotencyKey,
		Runner:              op.runnerName,
		EgressPorts:         op.egressPorts,
	}
	if op.timeout > 0 {
		req.Timeout = op.timeout.String()
	}

	job, err := rc.RunPlaybook(cmd.Context(), req)
	if err != nil {
		return 0, remoteFail(cmd, mode, err)
	}
	if op.noFollow {
		return 0, renderRemoteEnqueued(cmd, mode, job)
	}
	if mode != outputJSON {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(),
			"job %d queued for the remote daemon\n", job.JobID); err != nil {
			return 0, err
		}
	}
	code, err := followRemoteJob(cmd, rc, mode, job.JobID)
	if err != nil {
		return 0, remoteFail(cmd, mode, err)
	}
	return code, nil
}

// runRemoteWorkspace enqueues a plan or an apply on the remote daemon. Neither
// follows (see this file's header); an apply may answer gated instead.
func runRemoteWorkspace(cmd *cobra.Command, rc *client.Client, op workspaceOp) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	req := client.WorkspaceRequest{
		RunSecrets:     op.runSecrets,
		Timeout:        op.timeout,
		Plan:           op.planRef,
		IdempotencyKey: op.idempotencyKey,
		Runner:         op.runnerName,
	}

	if op.op == "plan" {
		job, err := rc.PlanWorkspace(cmd.Context(), op.workspace, req)
		if err != nil {
			return remoteFail(cmd, mode, err)
		}
		return renderRemoteEnqueued(cmd, mode, job)
	}

	if op.op == "destroy" {
		job, err := rc.DestroyWorkspace(cmd.Context(), op.workspace, req)
		if err != nil {
			return remoteFail(cmd, mode, err)
		}
		return renderRemoteEnqueued(cmd, mode, job)
	}

	if err := remotePlanRef(op.planRef); err != nil {
		return err
	}
	res, err := rc.ApplyWorkspace(cmd.Context(), op.workspace, req)
	if err != nil {
		return remoteFail(cmd, mode, err)
	}
	if res.Pending != nil {
		return renderRemotePending(cmd, mode, *res.Pending)
	}
	return renderRemoteEnqueued(cmd, mode, *res.Enqueued)
}

// runRemoteDecide settles an approval on the remote daemon. The capability token
// is derived there, so nothing bearer-like leaves this box.
func runRemoteDecide(cmd *cobra.Command, rc *client.Client, id int64, decision string) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	d, err := rc.DecideApproval(cmd.Context(), id, decision)
	if err != nil {
		return remoteFail(cmd, mode, err)
	}
	return renderRemoteDecision(cmd, mode, d)
}

// --- guards: flags that name something on THIS machine ----------------------

// A remote write resolves its inputs on the daemon: a playbook path, an
// inventory path or an -e @file are read there, not here. Silently sending one
// would run against whatever happens to sit at that path on the other box (or
// nothing), so each is refused with the substitute named.

// looksLikePath reports whether a value is a filesystem path rather than a
// registered name: anything with a separator, or a relative marker.
func looksLikePath(v string) bool {
	return strings.ContainsRune(v, '/') || v == "." || v == ".." ||
		strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || strings.HasPrefix(v, "~")
}

// remoteRunGuards refuses the `playbook run` inputs that cannot cross the wire.
func remoteRunGuards(playbook, inventory string, extraVars []string, source string, wait bool) error {
	if source == "" && !strings.HasPrefix(playbook, "/") {
		return fmt.Errorf(
			"playbook run: %q is a path on this machine; with --remote pass --source <name> "+
				"so the daemon resolves it in that source's checkout, or give an absolute path on the daemon",
			playbook)
	}
	if inventory != "" && looksLikePath(inventory) {
		return fmt.Errorf(
			"playbook run: --inventory %q is a path on this machine; with --remote name a "+
				"registered inventory the daemon can resolve", inventory)
	}
	for _, v := range extraVars {
		if strings.HasPrefix(v, "@") {
			return fmt.Errorf(
				"playbook run: --extra-var %q reads a file on this machine; with --remote pass "+
					"key=val, which travels in the request", v)
		}
	}
	if wait {
		return errors.New(
			"playbook run: --wait drives a hooked source's composed workflow, which the write " +
				"API does not offer; drop it and follow the job instead")
	}
	return nil
}

// remotePlanRef refuses a --plan that is not a saved artifact id.
func remotePlanRef(planRef string) error {
	if planRef == "" {
		return nil
	}
	for _, r := range planRef {
		if r < '0' || r > '9' {
			return fmt.Errorf(
				"workspace apply: --plan %q is not a saved plan artifact id; with --remote a plan "+
					"is named by id, because a path names a file on this machine", planRef)
		}
	}
	return nil
}
