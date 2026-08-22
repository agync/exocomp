package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// refreshInterval is how often the active screen reloads from the store.
const refreshInterval = 2 * time.Second

// tickMsg drives the periodic refresh of whichever screen is active.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// jobsLoadedMsg carries the result of a jobs list refresh.
type jobsLoadedMsg struct {
	jobs []store.Job
	err  error
}

func loadJobsCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
		if err != nil {
			return jobsLoadedMsg{err: err}
		}
		// ListJobs orders oldest first; the jobs screen shows the most
		// recent jobs first.
		reversed := make([]store.Job, len(jobs))
		for i, j := range jobs {
			reversed[len(jobs)-1-i] = j
		}
		if len(reversed) > maxJobsShown {
			reversed = reversed[:maxJobsShown]
		}
		return jobsLoadedMsg{jobs: reversed}
	}
}

const maxJobsShown = 200

// backupsLoadedMsg carries the result of a backup targets + latest run
// refresh.
type backupsLoadedMsg struct {
	targets []store.BackupTarget
	runs    map[int64]store.BackupRun
	err     error
}

func loadBackupsCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		targets, err := st.ListBackupTargets(ctx, true)
		if err != nil {
			return backupsLoadedMsg{err: err}
		}
		runs := make(map[int64]store.BackupRun, len(targets))
		for _, target := range targets {
			last, err := st.ListBackupRuns(ctx, target.ID, 1)
			if err != nil {
				return backupsLoadedMsg{err: err}
			}
			if len(last) > 0 {
				runs[target.ID] = last[0]
			}
		}
		return backupsLoadedMsg{targets: targets, runs: runs}
	}
}

// workspaceTofuConcurrencyKey is the concurrency_key `workspace plan` and
// `workspace apply` both enqueue under (cmd/exocomp/workspace.go); it is the
// only thing tying a job row back to a workspace, so it is also how this
// screen finds a workspace's tofu jobs.
func workspaceTofuConcurrencyKey(name string) string { return "tofu:" + name }

// workspacesLoadedMsg carries the result of a workspaces + tofu job status
// refresh.
type workspacesLoadedMsg struct {
	workspaces []store.Workspace
	status     map[int64]workspaceStatus // keyed by workspace id
	err        error
}

func loadWorkspacesCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		workspaces, err := st.ListWorkspaces(ctx)
		if err != nil {
			return workspacesLoadedMsg{err: err}
		}
		status := make(map[int64]workspaceStatus, len(workspaces))
		for _, ws := range workspaces {
			s, err := loadWorkspaceStatus(ctx, st, ws)
			if err != nil {
				return workspacesLoadedMsg{err: err}
			}
			status[ws.ID] = s
		}
		return workspacesLoadedMsg{workspaces: workspaces, status: status}
	}
}

// loadWorkspaceStatus derives one workspace's row status from its tofu jobs.
// ListJobs returns them oldest first, so the last plan and last apply seen
// while scanning are the most recent of each, and the final job scanned is
// the latest of either kind, which is what Enter tails. Drift comes from that
// last plan's own outcome event (executor.TofuPlanChanges).
func loadWorkspaceStatus(ctx context.Context, st *store.Store, ws store.Workspace) (workspaceStatus, error) {
	jobs, err := st.ListJobs(ctx, store.JobFilter{ConcurrencyKey: workspaceTofuConcurrencyKey(ws.Name)})
	if err != nil {
		return workspaceStatus{}, err
	}

	var (
		out        workspaceStatus
		lastPlanID int64
	)
	for _, j := range jobs {
		out.LatestJobID = j.ID
		if len(j.Args.Argv) < 2 {
			continue
		}
		switch j.Args.Argv[1] {
		case "plan":
			out.LastPlanState = j.State
			lastPlanID = j.ID
		case "apply":
			out.LastApplyState = j.State
		}
	}
	if lastPlanID == 0 {
		return out, nil
	}

	events, err := st.ListJobEvents(ctx, lastPlanID)
	if err != nil {
		return workspaceStatus{}, err
	}
	for _, e := range events {
		if changes, ok := executor.TofuPlanChanges(e.RawJSON); ok {
			out.Drift = changes
			break
		}
	}
	return out, nil
}

// scheduleLast is a schedule's last-fire summary: the state of the job its last
// fire enqueued and when that job ran, so the schedules screen shows both the
// outcome (a drift-check schedule's last drift result included) and its recency.
type scheduleLast struct {
	state store.JobState
	fired time.Time
}

// schedulesLoadedMsg carries the result of a schedules + last-fire refresh.
type schedulesLoadedMsg struct {
	schedules []store.Schedule
	last      map[int64]scheduleLast // keyed by schedule id
	err       error
}

func loadSchedulesCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		schedules, err := st.ListSchedules(ctx, false)
		if err != nil {
			return schedulesLoadedMsg{err: err}
		}
		last := make(map[int64]scheduleLast, len(schedules))
		for _, sc := range schedules {
			if sc.LastJobID == nil {
				continue
			}
			job, err := st.GetJob(ctx, *sc.LastJobID)
			if err != nil {
				return schedulesLoadedMsg{err: err}
			}
			l := scheduleLast{state: job.State}
			// The last fire's time is when its job ran: finished if it has,
			// else when it was queued (a still-running or queued fire).
			if job.FinishedAt != nil {
				l.fired = *job.FinishedAt
			} else {
				l.fired = job.QueuedAt
			}
			last[sc.ID] = l
		}
		return schedulesLoadedMsg{schedules: schedules, last: last}
	}
}

// sourcesLoadedMsg carries the result of a run-tree sources refresh.
type sourcesLoadedMsg struct {
	sources []store.Source
	err     error
}

func loadSourcesCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		sources, err := st.ListSources(context.Background())
		if err != nil {
			return sourcesLoadedMsg{err: err}
		}
		return sourcesLoadedMsg{sources: sources}
	}
}

// eventsLoadedMsg carries a job's event tail. It is keyed by jobID so a
// response arriving after the user has moved to a different job is ignored.
type eventsLoadedMsg struct {
	jobID  int64
	events []store.JobEvent
	err    error
}

func loadEventsCmd(st *store.Store, jobID int64) tea.Cmd {
	return func() tea.Msg {
		events, err := st.ListJobEvents(context.Background(), jobID)
		if err != nil {
			return eventsLoadedMsg{jobID: jobID, err: err}
		}
		return eventsLoadedMsg{jobID: jobID, events: events}
	}
}
