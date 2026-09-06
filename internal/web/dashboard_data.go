package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// banner reads the active reactive silences and notification mutes into the
// pause/silence/mute state that rides every screen, matching the TUI's model. A
// global silence is the kill switch (PAUSED); a scoped one is a SILENCED banner;
// an active notify_mute is MUTED — a distinct, milder hold where jobs still run
// and only the phone push is held. Any of them can hold at once.
func (s *Server) banner(r *http.Request) (bannerVM, error) {
	sils, err := s.store.ListSilences(r.Context(), true)
	if err != nil {
		return bannerVM{}, err
	}
	var b bannerVM
	var earliest time.Time
	for _, sil := range sils {
		if sil.ScopeKind == store.ScopeGlobal {
			b.Paused = true
		} else {
			b.Silenced = true
		}
		if earliest.IsZero() || sil.CreatedAt.Before(earliest) {
			earliest = sil.CreatedAt
		}
	}

	mutes, err := s.store.ListNotifyMutes(r.Context(), true)
	if err != nil {
		return bannerVM{}, err
	}
	if len(mutes) > 0 {
		b.Muted = true
		soonest := mutes[0].ExpiresAt
		for _, m := range mutes[1:] {
			if m.ExpiresAt.Before(soonest) {
				soonest = m.ExpiresAt
			}
		}
		b.MutedUntil = soonest.Local().Format("15:04")
	}

	// Swallowed counts alerts held only by a reactive silence/pause, not by a
	// notification mute: a mute does not swallow alerts, it holds the push.
	if b.Paused || b.Silenced {
		n, err := s.store.CountSuppressedSince(r.Context(), earliest)
		if err != nil {
			return bannerVM{}, err
		}
		b.Swallowed = n
	}
	return b, nil
}

// pendingApprovalsFrom is the needs-you list: every decision still open in the
// given approvals, with its runbook and fail-closed deadline. It takes the
// rows so a caller that already fetched the pending list for another purpose
// (the approvals page's fingerprint) does not query it a second time — the fp
// and the rendered rows must come from ONE fetch, or a change between two
// queries would leave a page whose fingerprint promised a state its rows did
// not show. A decision past half its window is marked expiring, the same
// threshold the re-publish push fires on.
func (s *Server) pendingApprovalsFrom(r *http.Request, approvals []store.Approval) ([]approvalRow, error) {
	now := s.now()
	var out []approvalRow
	for _, a := range approvals {
		row := approvalRow{
			ID: a.ID, JobID: a.JobID, Workspace: a.Workspace,
			Deadline:  reactive.ApprovalDeadline(a, now),
			ExpiresAt: a.ExpiresAt,
		}
		if job, err := s.store.GetJob(r.Context(), a.JobID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		} else {
			row.Runbook = job.Runbook
		}
		half := a.RequestedAt.Add(a.ExpiresAt.Sub(a.RequestedAt) / 2)
		row.Expiring = !now.Before(half)
		out = append(out, row)
	}
	return out, nil
}

// approvalSummaryFrom reduces the full pending-approvals list to the
// dashboard card: the open count and the soonest-to-expire item, the one
// line the card is allowed to show.
func approvalSummaryFrom(pending []approvalRow) approvalSummary {
	card := approvalSummary{Pending: len(pending)}
	if len(pending) == 0 {
		return card
	}
	soonest := pending[0]
	for _, a := range pending[1:] {
		if a.ExpiresAt.Before(soonest.ExpiresAt) {
			soonest = a
		}
	}
	card.Runbook = soonest.Runbook
	card.Workspace = soonest.Workspace
	card.Deadline = soonest.Deadline
	card.Expiring = soonest.Expiring
	return card
}

// jobFailedRecentWindow is how far back the dashboard's "failed recently"
// subline looks, the same 24h span the backup freshness ladder starts at.
const jobFailedRecentWindow = 24 * time.Hour

// jobSummary reads only the active queue — queued, awaiting-approval, and
// running jobs — never settled history, which stays the /jobs view's job. The
// States filter keeps this a narrow index lookup no matter how large the
// terminal job history has grown, and the recent-failure count is a single
// COUNT query rather than a list pulled into memory to be len()'d.
func (s *Server) jobSummary(r *http.Request) (jobSummary, error) {
	active, err := s.store.ListJobs(r.Context(), store.JobFilter{
		States: []store.JobState{store.StateQueued, store.StateAwaitingApproval, store.StateRunning},
	})
	if err != nil {
		return jobSummary{}, err
	}
	card := jobSummary{}
	for _, j := range active {
		if j.State == store.StateRunning {
			card.Running++
		} else {
			card.Queued++
		}
	}
	failed, err := s.store.CountFailedJobsSince(r.Context(), s.now().Add(-jobFailedRecentWindow))
	if err != nil {
		return jobSummary{}, err
	}
	card.FailedRecently = failed
	return card, nil
}

// backups reads each enabled target's newest run into a freshness row on the
// 24h/48h ladder.
func (s *Server) backups(r *http.Request) ([]backupRow, error) {
	targets, err := s.store.ListBackupTargets(r.Context(), false)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []backupRow
	for _, t := range targets {
		row := backupRow{Name: t.Name, Kind: t.Kind, State: "never", Age: "never", Freshness: "wait"}
		runs, err := s.store.ListBackupRuns(r.Context(), t.ID, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			run := runs[0]
			row.State = run.State
			row.Snapshot = run.SnapshotID
			when := run.StartedAt
			if run.FinishedAt != nil {
				when = *run.FinishedAt
			}
			age := now.Sub(when)
			row.Age = humanAge(age)
			row.Freshness = backupFreshness(age)
			if run.State != "succeeded" {
				row.Freshness = "fail"
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// workspaces reports each workspace's serial and whether it has a pending
// drift approval outstanding. A workspace named by an open approval is drifting
// — a plan found changes and is waiting on a human.
func (s *Server) workspaces(r *http.Request, pending []approvalRow) ([]workspaceRow, error) {
	list, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nil, err
	}
	drifting := map[string]bool{}
	for _, a := range pending {
		if a.Workspace != "" {
			drifting[a.Workspace] = true
		}
	}
	var out []workspaceRow
	for _, w := range list {
		out = append(out, workspaceRow{Name: w.Name, Serial: w.Serial, Drift: drifting[w.Name]})
	}
	return out, nil
}

// workspaceSummary reduces the workspace list to the dashboard card: the
// managed total and how many carry an open drift approval.
func (s *Server) workspaceSummary(r *http.Request, pending []approvalRow) (workspaceSummary, error) {
	list, err := s.workspaces(r, pending)
	if err != nil {
		return workspaceSummary{}, err
	}
	card := workspaceSummary{Total: len(list)}
	for _, w := range list {
		if w.Drift {
			card.Drifted++
		}
	}
	return card, nil
}

// sources reads the run-tree sources (ADR 0010) into rows: where each run's
// tree comes from, whether its ref is signed and pinned to allowed-signers, and
// the commit its last sync landed on. Read-only — registration is a
// CLI-and-local-auth path, not a web mutation.
func (s *Server) sources(r *http.Request) ([]sourceRow, error) {
	list, err := s.store.ListSources(r.Context())
	if err != nil {
		return nil, err
	}
	var out []sourceRow
	for _, src := range list {
		out = append(out, sourceRow{
			Name: src.Name, Kind: src.Kind, URI: src.URI, Ref: src.Ref,
			SyncSHA: src.LastSyncSHA, Signed: src.Signed,
			Signers: src.AllowedSigners != "", Enabled: src.Enabled,
		})
	}
	return out, nil
}

// sourceSummary reduces the source list to the dashboard card: the
// registered total and how many need attention — never synced, or disabled.
func (s *Server) sourceSummary(r *http.Request) (sourceSummary, error) {
	list, err := s.sources(r)
	if err != nil {
		return sourceSummary{}, err
	}
	card := sourceSummary{Total: len(list)}
	for _, src := range list {
		if !src.Enabled || src.NeverSynced() {
			card.Attention++
		}
		if src.NeverSynced() {
			card.NeverSynced++
			if len(card.NeverNames) < 2 {
				card.NeverNames = append(card.NeverNames, src.Name)
			}
		}
	}
	return card, nil
}

// checkSummary reduces the check estate to the dashboard card: the registered
// total, how many targets the sweep currently probes up, and how many checks
// are disabled. The up-target line is a single aggregate COUNT over
// check_state — never a per-check ListCheckStates loop, so the glance screen
// stays N+1-free. A target that reads down (or was never observed) is the
// attention fact the NEEDS ATTENTION band leads with — the rail badge counts
// down targets, so the band and the badge share one source of truth.
func (s *Server) checkSummary(r *http.Request) (checksSummary, error) {
	list, err := s.store.ListChecks(r.Context(), false)
	if err != nil {
		return checksSummary{}, err
	}
	card := checksSummary{Total: len(list)}
	targets := 0
	for _, c := range list {
		if !c.Enabled {
			card.Disabled++
			continue
		}
		targets += len(c.Targets)
	}
	card.TargetsTotal = targets
	up, err := s.store.CountCheckStatesByStatus(r.Context(), store.CheckUp)
	if err != nil {
		return checksSummary{}, err
	}
	card.TargetsUp = up
	down, err := s.store.CountCheckStatesByStatus(r.Context(), store.CheckDown)
	if err != nil {
		return checksSummary{}, err
	}
	card.TargetsDown = down
	return card, nil
}

// schedules reads every schedule (enabled and disabled) with its kind, cron,
// next fire, and the outcome of the job its last fire enqueued. A drift-check
// schedule (a tofu-plan or play --check kind) thereby surfaces its last drift
// result — the last job's state — without opening the job.
func (s *Server) schedules(r *http.Request) ([]scheduleRow, error) {
	list, err := s.store.ListSchedules(r.Context(), false)
	if err != nil {
		return nil, err
	}
	var out []scheduleRow
	for _, sc := range list {
		row := scheduleRow{
			Name: sc.Name, Kind: sc.Kind, Cron: sc.CronExpr,
			Enabled: sc.Enabled, Next: "-", LastRun: "-",
			Argv: sc.Args.Argv, Target: sc.Args.Target,
			ConcurrencyKey: sc.ConcurrencyKey, JitterS: sc.JitterS,
			TolerateUnreachable: sc.Args.TolerateUnreachable,
		}
		if sc.NextAt != nil {
			row.Next = clock(*sc.NextAt)
		}
		if sc.LastRun != nil {
			row.LastRun = clock(*sc.LastRun)
		}
		if sc.LastJobID != nil {
			if job, err := s.store.GetJob(r.Context(), *sc.LastJobID); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
			} else {
				row.HasLast = true
				row.LastJobID = job.ID
				row.LastRunbook = job.Runbook
				row.LastState = job.State
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// scheduleSummary reduces the schedule list to the dashboard card: the
// registered total and how many are disabled. A count-only reduction — the
// /schedules page owns the full list with cron, next/last fire, and outcome.
func (s *Server) scheduleSummary(r *http.Request) (scheduleSummary, error) {
	list, err := s.store.ListSchedules(r.Context(), false)
	if err != nil {
		return scheduleSummary{}, err
	}
	card := scheduleSummary{Total: len(list)}
	for _, sc := range list {
		if !sc.Enabled {
			card.Disabled++
		}
	}
	return card, nil
}

// workflowRowFrom derives one chain's row from its ordered step ledger: the
// whole-workflow state (read from the steps, not stored), a done/total tally,
// and each step's status and job.
func workflowRowFrom(id string, steps []store.WorkflowStep) workflowRow {
	row := workflowRow{ID: id, State: store.WorkflowStatus(steps), Total: len(steps)}
	for _, st := range steps {
		if st.Status == store.StepSucceeded {
			row.Done++
		}
		row.Steps = append(row.Steps, workflowStepRow{
			StepNo: st.StepNo, Kind: st.Kind, Status: st.Status, JobID: st.JobID,
		})
	}
	return row
}

// inflightWorkflows reads the chains with a step still to run — the
// provision->onboard->converge and TF->ansible handoff ledgers that are live
// right now — capped at limit for the glance screen.
func (s *Server) inflightWorkflows(r *http.Request, limit int) ([]workflowRow, error) {
	ids, err := s.store.ListIncompleteWorkflows(r.Context())
	if err != nil {
		return nil, err
	}
	var out []workflowRow
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		steps, err := s.store.GetWorkflow(r.Context(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, workflowRowFrom(id, steps))
	}
	return out, nil
}

// recentWorkflows reads the most-recently-active chains, in-flight and finished
// together, for the workflows surface.
func (s *Server) recentWorkflows(r *http.Request, limit int) ([]workflowRow, error) {
	ids, err := s.store.ListWorkflowIDs(r.Context(), limit)
	if err != nil {
		return nil, err
	}
	var out []workflowRow
	for _, id := range ids {
		steps, err := s.store.GetWorkflow(r.Context(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, workflowRowFrom(id, steps))
	}
	return out, nil
}

// policies reads every CEL plan policy into rows: which workspace it gates,
// whether a match denies the plan or routes the apply through approval, its
// expression verbatim, and whether it is enabled.
func (s *Server) policies(r *http.Request) ([]policyRow, error) {
	list, err := s.store.ListPlanPolicies(r.Context(), "", false)
	if err != nil {
		return nil, err
	}
	var out []policyRow
	for _, p := range list {
		out = append(out, policyRow{
			Name: p.Name, Workspace: p.Workspace, Action: p.Action,
			Expr: p.Expr, Enabled: p.Enabled,
		})
	}
	return out, nil
}

// runners reads every registered runner definition (byo-runner.md §2). It is
// read-only: registration, import, and removal stay CLI verbs. The bootstrap
// script body is never read into the row — store.Runner carries it, but
// nothing here forwards it onward.
func (s *Server) runners(r *http.Request) ([]runnerRow, error) {
	list, err := s.store.ListRunners(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]runnerRow, 0, len(list))
	for _, rn := range list {
		out = append(out, runnerRow{
			Name: rn.Name, Mode: rn.Mode, Base: rn.Base,
			Packages: rn.Packages, AnsibleCore: rn.AnsibleCore,
			MemoryMax: rn.MemoryMax, CPUQuota: rn.CPUQuota,
			Hash:    rn.ContentHash,
			Created: clock(rn.CreatedAt),
		})
	}
	return out, nil
}

// secrets reads every sealed secret's lifecycle metadata and the run-secret
// bindings that inject it. It NEVER reads or renders the sealed value, nonce, or
// ciphertext — the SecretRow carries them but the row built here drops them
// (invariant 4). Read-only: creation, rotation and deletion stay CLI verbs.
// Bindings are grouped once by secret name, so each row's "used by" lines come
// from a map rather than a per-row query.
func (s *Server) secrets(r *http.Request) ([]secretRow, error) {
	list, err := s.store.ListSecrets(r.Context())
	if err != nil {
		return nil, err
	}
	bindings, err := s.store.ListRunSecretBindings(r.Context())
	if err != nil {
		return nil, err
	}
	byName := map[string][]secretBindingRow{}
	for _, b := range bindings {
		pairs, err := s.store.RunSecretsByBinding(r.Context(), b)
		if err != nil {
			return nil, err
		}
		for _, rs := range pairs {
			byName[rs.SecretName] = append(byName[rs.SecretName],
				secretBindingRow{Binding: rs.Binding, EnvName: rs.EnvName})
		}
	}
	now := s.now()
	var out []secretRow
	for _, sec := range list {
		out = append(out, secretRow{
			Name: sec.Name, Kind: sec.Kind, Source: sec.Source,
			Created:  clock(sec.CreatedAt),
			Rotated:  orNever(sec.RotatedAt),
			LastUsed: orNever(sec.LastUsedAt),
			Expiry:   secretExpiry(sec.ExpiresAt, now),
			Bindings: byName[sec.Name],
		})
	}
	return out, nil
}

// selfObs builds the dashboard's self-observability row: the running binary
// version (the upgrade target) and the Litestream replication RPO age. The RPO
// read mirrors the metrics registry's litestream gauge exactly — read the
// configured replica target, then observe the newest replicated frame's age —
// so the surface and the scrape never disagree. An age exocomp cannot observe
// (no replica, a remote replica only litestream can read, or no frames yet) is
// reported honestly as "unknown", never as a fresh-looking zero.
func (s *Server) selfObs(ctx context.Context) selfObs {
	so := selfObs{Version: s.version}
	if so.Version == "" {
		so.Version = "dev"
	}

	raw, err := s.store.GetSetting(ctx, litestream.SettingReplicaTarget)
	if err != nil || raw == "" {
		so.LSAge = "unknown"
		so.LSClass = "dim"
		so.LSDetail = "no replica configured"
		return so
	}
	so.LSConfigured = true

	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		so.LSAge = "unknown"
		so.LSClass = "wait"
		so.LSDetail = "replica target unparseable"
		return so
	}
	age, _, ok, ferr := litestream.ReplicaFreshness(target, s.now())
	if ferr != nil || !ok {
		so.LSAge = "unknown"
		so.LSClass = "dim"
		if target.Remote() {
			so.LSDetail = "remote replica — frame age readable only by litestream"
		} else {
			so.LSDetail = "no frames replicated yet"
		}
		return so
	}
	so.LSAge = humanAge(age)
	so.LSClass = rpoFreshness(age)
	so.LSDetail = "newest replicated frame"
	return so
}

// rpoFreshness maps a replication RPO age to the chip ladder. Litestream ships
// WAL frames continuously, so a homelab replica is fresh in minutes: quiet
// under 10m, a warning past it, loud past an hour of unreplicated writes.
func rpoFreshness(age time.Duration) string {
	switch {
	case age >= time.Hour:
		return "fail"
	case age >= 10*time.Minute:
		return "wait"
	default:
		return "ok"
	}
}

// signal builds the powerline's is-the-control-plane-alive strip: newest backup
// age (the dead-man), push transport presence, and disk headroom. Each segment
// also carries the signal state the strip renders in (F10): a dead-man with no
// backup is a wait state with a fix verb, ntfy-off is deliberate dim, disk is
// ok until it threatens. The dead-man reads the store's ONE rule
// (LastSucceededBackupTime): succeeded runs only, always finished — a failed or
// still-running newest run never masks an older good backup.
func (s *Server) signal(ctx context.Context) SignalLine {
	sig := SignalLine{DeadMan: "—", Ntfy: "off", Disk: "—"}
	sig.NtfyState = "dim"

	if when, ok := s.store.LastSucceededBackupTime(ctx); ok {
		sig.DeadMan = humanAge(s.now().Sub(when))
		sig.DeadManState = "ok"
	} else {
		sig.DeadMan = "none"
		sig.DeadManState = "wait"
	}

	if _, err := s.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
		sig.Ntfy = "on"
		sig.NtfyState = "ok"
	}

	if s.dataDir != "" {
		if pct, ok := diskFreePercent(s.dataDir); ok {
			sig.Disk = fmt.Sprintf("%d%% free", pct)
			sig.DiskState = "ok"
			if pct < 10 {
				sig.DiskState = "fail"
			}
		}
	}
	return sig
}

// diskFreePercent returns the free-space percentage of the filesystem holding
// path.
func diskFreePercent(path string) (int, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0, false
	}
	return int(free * 100 / total), true
}
