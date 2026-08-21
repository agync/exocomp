package tui

import (
	"fmt"

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
	jobsTab := "[1] Jobs"
	backupsTab := "[2] Backups"
	if m.active == screenJobs {
		jobsTab = styleTabActive.Render(jobsTab)
		backupsTab = styleTabInactive.Render(backupsTab)
	} else {
		jobsTab = styleTabInactive.Render(jobsTab)
		backupsTab = styleTabActive.Render(backupsTab)
	}
	return styleTitle.Render("exocomp") + "   " + jobsTab + "   " + backupsTab
}

func (m Model) renderBody() string {
	switch {
	case m.tailing:
		return m.renderTail()
	case m.active == screenJobs:
		return m.renderJobs()
	default:
		return m.renderBackups()
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
	default:
		return styleHelp.Render("[1]/[2] switch   tab switch   enter tail job   q quit")
	}
}
