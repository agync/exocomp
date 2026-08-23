package tui

import (
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/store"
)

// screen selects which top-level view is showing. [0] Home is the root; [1]-[4]
// are the M1 tables; [5] Approvals and [6] Events are the reactive surfaces;
// [7] Sources, [8] Policies and [9] Workflows are the post-v1 parity surfaces.
type screen int

const (
	screenHome       screen = iota // [0] the glance, and the root: q quits only here
	screenJobs                     // [1]
	screenBackups                  // [2]
	screenWorkspaces               // [3]
	screenSchedules                // [4]
	screenApprovals                // [5]
	screenEvents                   // [6]
	screenSources                  // [7] post-v1 parity surfaces
	screenPolicies                 // [8]
	screenWorkflows                // [9]
	screenSecrets                  // [s] — the digits are spent, so the read-only secrets surface takes a mnemonic key
	screenRunners                  // [r] — digits spent too; the read-only runner registry takes a mnemonic key
)

const (
	headerLines = 2 // title/tabs line + blank
	helpLines   = 2 // blank + help line
)

// Model is the top-level Bubble Tea program: the Home glance ([0], the root),
// the M1 tables (Jobs, Backups, Workspaces, Schedules; [1]-[4]/tab), the
// Approvals ([5]) and Events ([6]) surfaces, the post-v1 parity surfaces
// (Sources [7], Policies [8], Workflows [9] with a step-ledger detail), a job
// event tail entered from the Jobs and Workspaces screens, and a hold banner.
// It refreshes from the store on a 2s tick. q quits only at the root; esc walks
// a screen back.
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

	schedules      []store.Schedule
	scheduleLast   map[int64]scheduleLast
	schedulesTable schedulesTable
	schedulesErr   error

	// Post-v1 parity surfaces the web UI grew after v1 that the TUI had not.
	sources    []store.Source
	sourcesErr error

	policies    []store.PlanPolicy
	policiesErr error

	workflows       []workflowView
	workflowsErr    error
	workflowsCursor int
	workflowDetail  string // non-empty: the workflow id whose step ledger is open

	// The read-only secrets surface: sealed-secret metadata and, keyed by
	// secret name, the run-secret bindings that inject each one. No value,
	// nonce or ciphertext is ever held here.
	secrets        []store.SecretRow
	secretBindings map[string][]store.RunSecret
	secretsErr     error

	// The read-only runner registry: byo-runner definitions. No bootstrap
	// body or Containerfile is ever held here — only what the columns show.
	runners    []store.Runner
	runnersErr error

	// The glance streams the new screens read: pending approvals, recent
	// events, and the active holds behind the banner.
	approvals      []store.Approval
	reactiveEvents []store.Event
	silences       []store.Silence
	swallowed      int
	mutes          []store.NotifyMute
	selfRPO        string
	selfDetail     string
	glanceErr      error

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
		active:          screenHome,
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
		loadSourcesCmd(m.store),
		loadPoliciesCmd(m.store),
		loadWorkflowsCmd(m.store),
		loadSecretsCmd(m.store),
		loadRunnersCmd(m.store),
		loadGlanceCmd(m.store),
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
			m.scheduleLast = msg.last
			m.schedulesTable.SetRows(scheduleRows(msg.schedules, msg.last))
		}
		return m, nil

	case sourcesLoadedMsg:
		m.sourcesErr = msg.err
		if msg.err == nil {
			m.sources = msg.sources
		}
		return m, nil

	case policiesLoadedMsg:
		m.policiesErr = msg.err
		if msg.err == nil {
			m.policies = msg.policies
		}
		return m, nil

	case workflowsLoadedMsg:
		m.workflowsErr = msg.err
		if msg.err == nil {
			m.workflows = msg.workflows
			// Clamp the cursor: a reload may have shortened the list.
			if m.workflowsCursor >= len(m.workflows) {
				m.workflowsCursor = 0
			}
		}
		return m, nil

	case secretsLoadedMsg:
		m.secretsErr = msg.err
		if msg.err == nil {
			m.secrets = msg.secrets
			m.secretBindings = msg.bindings
		}
		return m, nil

	case runnersLoadedMsg:
		m.runnersErr = msg.err
		if msg.err == nil {
			m.runners = msg.runners
		}
		return m, nil

	case glanceLoadedMsg:
		m.glanceErr = msg.err
		if msg.err == nil {
			m.approvals = msg.approvals
			m.reactiveEvents = msg.events
			m.silences = msg.silences
			m.mutes = msg.mutes
			m.swallowed = msg.swallowed
			m.selfRPO = msg.selfRPO
			m.selfDetail = msg.selfDetail
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
	case screenHome:
		// The glance reads every stream: reload them all.
		cmds = append(cmds,
			loadJobsCmd(m.store), loadBackupsCmd(m.store),
			loadWorkspacesCmd(m.store), loadSchedulesCmd(m.store), loadGlanceCmd(m.store))
	case screenJobs:
		cmds = append(cmds, loadJobsCmd(m.store))
	case screenBackups:
		cmds = append(cmds, loadBackupsCmd(m.store))
	case screenWorkspaces:
		cmds = append(cmds, loadWorkspacesCmd(m.store))
	case screenSchedules:
		cmds = append(cmds, loadSchedulesCmd(m.store))
	case screenSources:
		cmds = append(cmds, loadSourcesCmd(m.store))
	case screenPolicies:
		cmds = append(cmds, loadPoliciesCmd(m.store))
	case screenWorkflows:
		cmds = append(cmds, loadWorkflowsCmd(m.store))
	case screenSecrets:
		cmds = append(cmds, loadSecretsCmd(m.store))
	case screenRunners:
		cmds = append(cmds, loadRunnersCmd(m.store))
	case screenApprovals, screenEvents:
		cmds = append(cmds, loadGlanceCmd(m.store))
	}
	if m.tailing {
		cmds = append(cmds, loadEventsCmd(m.store, m.tailJobID))
	}
	return tea.Batch(cmds...)
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "q":
		// q quits only at the root; elsewhere esc walks back. This is the
		// keymap delta the design settled on, so a stray q deep in a screen
		// cannot drop the operator out of the whole UI.
		if !m.tailing && m.active == screenHome {
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil

	case "0":
		if !m.tailing {
			m.active = screenHome
		}
		return m, nil

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

	case "5":
		if !m.tailing {
			m.active = screenApprovals
		}
		return m, nil

	case "6":
		if !m.tailing {
			m.active = screenEvents
		}
		return m, nil

	case "7":
		if !m.tailing {
			m.active = screenSources
		}
		return m, nil

	case "8":
		if !m.tailing {
			m.active = screenPolicies
		}
		return m, nil

	case "9":
		if !m.tailing {
			m.active = screenWorkflows
			m.workflowDetail = ""
		}
		return m, nil

	case "s":
		// The 0-9 digits are spent on Home and the nine screens, so the
		// read-only secrets surface takes a mnemonic key. Like the digits it is
		// a global jump, inert while a job tail is open.
		if !m.tailing {
			m.active = screenSecrets
		}
		return m, nil

	case "r":
		// Like [s], a mnemonic global jump: the digits are spent, and it is
		// inert while a job tail is open.
		if !m.tailing {
			m.active = screenRunners
		}
		return m, nil

	case "tab":
		if !m.tailing {
			m.active = otherScreen(m.active)
		}
		return m, nil

	case "esc":
		switch {
		case m.tailing:
			m.tailing = false
			m.tailJobID = 0
			m.tailEvents = nil
			m.tailErr = nil
		case m.active == screenWorkflows && m.workflowDetail != "":
			// esc closes the step ledger back to the workflows list.
			m.workflowDetail = ""
		case m.active != screenHome:
			// esc walks back toward the root rather than quitting.
			m.active = screenHome
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
		case screenWorkflows:
			if m.workflowDetail == "" && m.workflowsCursor < len(m.workflows) {
				m.workflowDetail = m.workflows[m.workflowsCursor].id
			}
			return m, nil
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
	case m.active == screenWorkflows && m.workflowDetail == "":
		m.moveWorkflowsCursor(msg.String())
	}
	return m, cmd
}

// moveWorkflowsCursor walks the workflows list selection. The surface is a
// fixture frame, not a bubbles table, so it carries its own j/k + arrow cursor.
func (m *Model) moveWorkflowsCursor(key string) {
	switch key {
	case "up", "k":
		if m.workflowsCursor > 0 {
			m.workflowsCursor--
		}
	case "down", "j":
		if m.workflowsCursor < len(m.workflows)-1 {
			m.workflowsCursor++
		}
	}
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
