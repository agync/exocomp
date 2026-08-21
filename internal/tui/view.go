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
	return tea.NewView(m.renderHeader() + "\n\n" + m.renderBody() + "\n\n" + m.renderHelp())
}

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
