package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// View implements tea.Model.
func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	// The v0.2 screens are full frames the fixture renderers draw end to end
	// (their own header, rule, and help bar); the M1 screens still use the
	// generic header/body/help chrome. A banner, when a hold is active, rides
	// above whichever is showing.
	if !m.tailing {
		switch m.active {
		case screenHome:
			return tea.NewView(liveFrame(m.withBanner(m.renderHomeScreen())))
		case screenApprovals:
			return tea.NewView(liveFrame(m.withBanner(renderApprovalDetail(m.approvalsModel()))))
		case screenEvents:
			return tea.NewView(liveFrame(m.withBanner(renderEventsScreen(m.eventsModel()))))
		case screenSources:
			return tea.NewView(liveFrame(m.withBanner(m.renderSourcesScreen())))
		case screenWorkflows:
			return tea.NewView(liveFrame(m.withBanner(m.renderWorkflowsScreen())))
		}
	}
	return tea.NewView(m.renderHeader() + "\n\n" + m.renderBody() + "\n\n" + m.renderHelp())
}

// renderHomeScreen picks the width-appropriate Home layout.
func (m Model) renderHomeScreen() string {
	if m.width >= 120 {
		return renderHome120(m.homeModel())
	}
	return renderHome80(m.homeModel())
}

// renderSourcesScreen renders the sources surface, or its error/empty state.
func (m Model) renderSourcesScreen() string {
	if m.sourcesErr != nil {
		return styleErr.Render("failed to load sources: " + m.sourcesErr.Error())
	}
	return renderSourcesScreen(m.sourcesModel())
}

// renderWorkflowsScreen renders the workflows list, or the step ledger of the
// chain whose detail is open, or the error state.
func (m Model) renderWorkflowsScreen() string {
	if m.workflowsErr != nil {
		return styleErr.Render("failed to load workflows: " + m.workflowsErr.Error())
	}
	if m.workflowDetail != "" {
		return renderWorkflowDetail(m.workflowDetailModel())
	}
	return renderWorkflowsScreen(m.workflowsModel())
}

// withBanner prepends the hold banner when a silence or mute is active.
func (m Model) withBanner(screen string) string {
	if b := m.bannerState(); b.Active() {
		return bannerLine(b) + "\n" + screen
	}
	return screen
}

// liveFrame strips the trailing newline the fixture renderers carry: a golden
// file ends in one, but a full-height frame handed to the program with a
// trailing newline scrolls its own first line off the top of the terminal.
func liveFrame(s string) string { return strings.TrimSuffix(s, "\n") }

func (m Model) renderHeader() string {
	tabs := []struct {
		screen screen
		label  string
	}{
		{screenJobs, "[1] Jobs"},
		{screenBackups, "[2] Backups"},
		{screenWorkspaces, "[3] Workspaces"},
		{screenSchedules, "[4] Schedules"},
	}
	rendered := make([]string, len(tabs))
	for i, tab := range tabs {
		if tab.screen == m.active {
			rendered[i] = styleTabActive.Render(tab.label)
		} else {
			rendered[i] = styleTabInactive.Render(tab.label)
		}
	}
	return styleTitle.Render("exocomp") + "   " + strings.Join(rendered, "   ")
}

func (m Model) renderBody() string {
	switch {
	case m.tailing:
		return m.renderTail()
	case m.active == screenJobs:
		return m.renderJobs()
	case m.active == screenBackups:
		return m.renderBackups()
	case m.active == screenWorkspaces:
		return m.renderWorkspaces()
	default:
		return m.renderSchedules()
	}
}

func (m Model) renderJobs() string {
	if m.jobsErr != nil {
		return styleErr.Render("failed to load jobs: " + m.jobsErr.Error())
	}
	if len(m.jobs) == 0 {
		return styleDim.Render("no jobs yet")
	}
	return m.jobsTable.View()
}

func (m Model) renderBackups() string {
	if m.backupsErr != nil {
		return styleErr.Render("failed to load backup targets: " + m.backupsErr.Error())
	}
	if len(m.targets) == 0 {
		return styleDim.Render("no backup targets configured")
	}
	return m.backupsTable.View()
}

func (m Model) renderWorkspaces() string {
	if m.workspacesErr != nil {
		return styleErr.Render("failed to load workspaces: " + m.workspacesErr.Error())
	}
	if len(m.workspaces) == 0 {
		return styleDim.Render("no workspaces registered")
	}
	return m.workspacesTable.View()
}

func (m Model) renderSchedules() string {
	if m.schedulesErr != nil {
		return styleErr.Render("failed to load schedules: " + m.schedulesErr.Error())
	}
	if len(m.schedules) == 0 {
		return styleDim.Render("no schedules configured")
	}
	return m.schedulesTable.View()
}

func (m Model) renderTail() string {
	title := fmt.Sprintf("job %d events", m.tailJobID)
	if m.tailErr != nil {
		return styleTitle.Render(title) + "\n\n" + styleErr.Render("failed to load events: "+m.tailErr.Error())
	}
	return styleTitle.Render(title) + "\n\n" + m.tailView.View()
}

func (m Model) renderHelp() string {
	switch {
	case m.tailing:
		return styleHelp.Render("esc back   q quit")
	case m.active == screenJobs || m.active == screenWorkspaces:
		return styleHelp.Render("[1]-[4] switch   tab switch   enter tail job   q quit")
	default:
		return styleHelp.Render("[1]-[4] switch   tab switch   q quit")
	}
}
