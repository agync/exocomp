package tui

import (
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/store"
)

// screen selects which of the four top-level views is showing.
type screen int

const (
	screenJobs screen = iota
	screenBackups
	screenWorkspaces
	screenSchedules
)

const (
	headerLines = 2 // title/tabs line + blank
	helpLines   = 2 // blank + help line
)

// Model is the top-level Bubble Tea program: four screens (Jobs, Backups,
// Workspaces, Schedules) switchable with 1/2/3/4/tab, a job event tail
// entered from the Jobs and Workspaces screens, and a bottom help bar. It
// refreshes from the store on a 2s tick.
type Model struct {
	store *store.Store

	active        screen
	width, height int

	jobs      []store.Job
	jobsTable jobsTable
	jobsErr   error

	targets      []store.BackupTarget
	runs         map[int64]store.BackupRun
	backupsTable backupsTable
	backupsErr   error

	workspaces      []store.Workspace
	workspaceStatus map[int64]workspaceStatus
	workspacesTable workspacesTable
	workspacesErr   error

	schedules         []store.Schedule
	scheduleLastState map[int64]store.JobState
	schedulesTable    schedulesTable
	schedulesErr      error

	tailing    bool
	tailJobID  int64
	tailView   viewport.Model
	tailEvents []store.JobEvent
	tailErr    error

	quitting bool
}

// New builds the program model over an already-open store.
func New(st *store.Store) Model {
	m := Model{
		store:           st,
		active:          screenJobs,
		width:           80,
		height:          24,
		jobsTable:       newJobsTable(),
		backupsTable:    newBackupsTable(),
		workspacesTable: newWorkspacesTable(),
		schedulesTable:  newSchedulesTable(),
		tailView:        viewport.New(viewport.WithWidth(80), viewport.WithHeight(16)),
		runs:            map[int64]store.BackupRun{},
	}
	m.jobsTable.Focus()
	m.backupsTable.Focus()
	m.workspacesTable.Focus()
	m.schedulesTable.Focus()
	m.layout()
	return m
}

// Init kicks off the first load of every screen and the refresh tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadJobsCmd(m.store),
		loadBackupsCmd(m.store),
		loadWorkspacesCmd(m.store),
		loadSchedulesCmd(m.store),
		tickCmd(),
	)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tickMsg:
		return m, m.refreshCmd()

	case jobsLoadedMsg:
		m.jobsErr = msg.err
		if msg.err == nil {
			m.jobs = msg.jobs
			m.jobsTable.SetRows(jobRows(msg.jobs))
		}
		return m, nil

	case backupsLoadedMsg:
		m.backupsErr = msg.err
		if msg.err == nil {
			m.targets = msg.targets
			m.runs = msg.runs
			m.backupsTable.SetRows(backupRows(msg.targets, msg.runs))
		}
		return m, nil

	case workspacesLoadedMsg:
		m.workspacesErr = msg.err
		if msg.err == nil {
			m.workspaces = msg.workspaces
			m.workspaceStatus = msg.status
			m.workspacesTable.SetRows(workspaceRows(msg.workspaces, msg.status))
		}
		return m, nil

	case schedulesLoadedMsg:
		m.schedulesErr = msg.err
		if msg.err == nil {
			m.schedules = msg.schedules
			m.scheduleLastState = msg.lastState
			m.schedulesTable.SetRows(scheduleRows(msg.schedules, msg.lastState))
		}
		return m, nil

	case eventsLoadedMsg:
		if msg.jobID != m.tailJobID {
			return m, nil
		}
		m.tailErr = msg.err
		if msg.err == nil {
			atBottom := m.tailView.AtBottom()
			grew := len(msg.events) > len(m.tailEvents)
			m.tailEvents = msg.events
			m.tailView.SetContent(renderEvents(msg.events))
			if atBottom && grew {
				m.tailView.GotoBottom()
			}
		}
		return m, nil
	}

	return m, nil
}

// refreshCmd reloads whatever the active screen needs, the job tail if one is
// open (tailing survives a screen switch, so this is independent of active),
// and reschedules the tick.
func (m Model) refreshCmd() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	switch m.active {
	case screenJobs:
		cmds = append(cmds, loadJobsCmd(m.store))
	case screenBackups:
		cmds = append(cmds, loadBackupsCmd(m.store))
	case screenWorkspaces:
		cmds = append(cmds, loadWorkspacesCmd(m.store))
	case screenSchedules:
		cmds = append(cmds, loadSchedulesCmd(m.store))
	}
	if m.tailing {
		cmds = append(cmds, loadEventsCmd(m.store, m.tailJobID))
	}
	return tea.Batch(cmds...)
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "1":
		if !m.tailing {
			m.active = screenJobs
		}
		return m, nil

	case "2":
		if !m.tailing {
			m.active = screenBackups
		}
		return m, nil

	case "3":
		if !m.tailing {
			m.active = screenWorkspaces
		}
		return m, nil

	case "4":
		if !m.tailing {
			m.active = screenSchedules
		}
		return m, nil

	case "tab":
		if !m.tailing {
			m.active = otherScreen(m.active)
		}
		return m, nil

	case "esc":
		if m.tailing {
			m.tailing = false
			m.tailJobID = 0
			m.tailEvents = nil
			m.tailErr = nil
		}
		return m, nil

	case "enter":
		if m.tailing {
			return m, nil
		}
		switch m.active {
		case screenJobs:
			return m.startTailSelectedJob()
		case screenWorkspaces:
			return m.startTailSelectedWorkspace()
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch {
	case m.tailing:
		m.tailView, cmd = m.tailView.Update(msg)
	case m.active == screenJobs:
		m.jobsTable.model, cmd = m.jobsTable.model.Update(msg)
	case m.active == screenBackups:
		m.backupsTable.model, cmd = m.backupsTable.model.Update(msg)
	case m.active == screenWorkspaces:
		m.workspacesTable.model, cmd = m.workspacesTable.model.Update(msg)
	case m.active == screenSchedules:
		m.schedulesTable.model, cmd = m.schedulesTable.model.Update(msg)
	}
	return m, cmd
}

// startTailSelectedJob begins tailing the Jobs screen's selected job.
// Selection is resolved against m.jobs rather than the table's string cells
// so it never depends on parsing rendered, styled text.
func (m Model) startTailSelectedJob() (tea.Model, tea.Cmd) {
	idx := m.jobsTable.model.Cursor()
	if idx < 0 || idx >= len(m.jobs) {
		return m, nil
	}
	return m.startTail(m.jobs[idx].ID)
}

// startTailSelectedWorkspace begins tailing the selected workspace's latest
// tofu job (plan or apply, whichever ran most recently). A workspace with no
// job yet has nothing to tail.
func (m Model) startTailSelectedWorkspace() (tea.Model, tea.Cmd) {
	idx := m.workspacesTable.model.Cursor()
	if idx < 0 || idx >= len(m.workspaces) {
		return m, nil
	}
	id := m.workspaceStatus[m.workspaces[idx].ID].LatestJobID
	if id == 0 {
		return m, nil
	}
	return m.startTail(id)
}

// startTail opens the event tail on the given job.
func (m Model) startTail(id int64) (tea.Model, tea.Cmd) {
	m.tailing = true
	m.tailJobID = id
	m.tailEvents = nil
	m.tailErr = nil
	m.tailView.SetContent("")
	m.tailView.GotoTop()
	return m, loadEventsCmd(m.store, id)
}

// otherScreen cycles Jobs -> Backups -> Workspaces -> Schedules -> Jobs.
func otherScreen(s screen) screen {
	switch s {
	case screenJobs:
		return screenBackups
	case screenBackups:
		return screenWorkspaces
	case screenWorkspaces:
		return screenSchedules
	default:
		return screenJobs
	}
}

func (m *Model) layout() {
	bodyHeight := m.height - headerLines - helpLines
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	m.jobsTable.SetSize(m.width, bodyHeight)
	m.backupsTable.SetSize(m.width, bodyHeight)
	m.workspacesTable.SetSize(m.width, bodyHeight)
	m.schedulesTable.SetSize(m.width, bodyHeight)
	m.tailView.SetWidth(m.width)
	m.tailView.SetHeight(bodyHeight - 1) // -1 for the tail's own title line
}
