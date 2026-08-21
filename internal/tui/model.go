package tui

import (
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/store"
)

// screen selects which of the two top-level views is showing.
type screen int

const (
	screenJobs screen = iota
	screenBackups
)

const (
	headerLines = 2 // title/tabs line + blank
	helpLines   = 2 // blank + help line
)

// Model is the top-level Bubble Tea program: two screens (Jobs, Backups)
// switchable with 1/2/tab, a job event tail entered from the Jobs screen,
// and a bottom help bar. It refreshes from the store on a 2s tick.
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
		store:        st,
		active:       screenJobs,
		width:        80,
		height:       24,
		jobsTable:    newJobsTable(),
		backupsTable: newBackupsTable(),
		tailView:     viewport.New(viewport.WithWidth(80), viewport.WithHeight(16)),
		runs:         map[int64]store.BackupRun{},
	}
	m.jobsTable.Focus()
	m.backupsTable.Focus()
	m.layout()
	return m
}

// Init kicks off the first load of both screens and the refresh tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadJobsCmd(m.store), loadBackupsCmd(m.store), tickCmd())
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

// refreshCmd reloads whatever the active screen needs and reschedules the
// tick.
func (m Model) refreshCmd() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	switch m.active {
	case screenJobs:
		cmds = append(cmds, loadJobsCmd(m.store))
		if m.tailing {
			cmds = append(cmds, loadEventsCmd(m.store, m.tailJobID))
		}
	case screenBackups:
		cmds = append(cmds, loadBackupsCmd(m.store))
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
		if m.active == screenJobs && !m.tailing {
			return m.startTail()
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
	}
	return m, cmd
}

// startTail begins tailing the currently selected job's events. Selection is
// resolved against m.jobs rather than the table's string cells so it never
// depends on parsing rendered, styled text.
func (m Model) startTail() (tea.Model, tea.Cmd) {
	idx := m.jobsTable.model.Cursor()
	if idx < 0 || idx >= len(m.jobs) {
		return m, nil
	}
	id := m.jobs[idx].ID

	m.tailing = true
	m.tailJobID = id
	m.tailEvents = nil
	m.tailErr = nil
	m.tailView.SetContent("")
	m.tailView.GotoTop()
	return m, loadEventsCmd(m.store, id)
}

func otherScreen(s screen) screen {
	if s == screenJobs {
		return screenBackups
	}
	return screenJobs
}

func (m *Model) layout() {
	bodyHeight := m.height - headerLines - helpLines
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	m.jobsTable.SetSize(m.width, bodyHeight)
	m.backupsTable.SetSize(m.width, bodyHeight)
	m.tailView.SetWidth(m.width)
	m.tailView.SetHeight(bodyHeight - 1) // -1 for the tail's own title line
}
