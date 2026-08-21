package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

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
