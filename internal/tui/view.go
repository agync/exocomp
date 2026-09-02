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
	// (their own header, rule, and help bar); the older screens still use the
	// generic header/body/help chrome. A banner, when a hold is active, rides
	// above whichever is showing.
	var base string
	if m.tailing {
		base = m.renderHeader() + "\n\n" + m.renderBody() + "\n\n" + m.renderHelp()
	} else {
		switch m.active {
		case screenHome:
			base = liveFrame(m.withBanner(m.renderHomeScreen()))
		case screenApprovals:
			am := m.approvalsModel()
			if am.stale.workspace != "" {
				// A stale plan is a refusal: no approve key, the re-plan or
				// reject path only.
				base = liveFrame(m.withBanner(renderStalePlan(am.stale)))
			} else {
				base = liveFrame(m.withBanner(renderApprovalDetail(am)))
			}
		case screenEvents:
			base = liveFrame(m.withBanner(renderEventsScreen(m.eventsModel())))
		case screenSources:
			base = liveFrame(m.withBanner(m.renderSourcesScreen()))
		case screenPolicies:
			base = liveFrame(m.withBanner(m.renderPoliciesScreen()))
		case screenWorkflows:
			base = liveFrame(m.withBanner(m.renderWorkflowsScreen()))
		case screenSecrets:
			base = liveFrame(m.withBanner(m.renderSecretsScreen()))
		case screenRunners:
			base = liveFrame(m.withBanner(m.renderRunnersScreen()))
		default:
			base = m.renderHeader() + "\n\n" + m.renderBody() + "\n\n" + m.renderHelp()
		}
	}

	// Overlays: help and the event trace replace the screen; an input modal or
	// a flash splices over it so the operator keeps the context behind it.
	if m.helpOpen {
		return tea.NewView(renderHelpOverlay())
	}
	if m.traceOpen {
		return tea.NewView(m.withBanner(m.traceContent))
	}
	if m.diffOpen {
		return tea.NewView(m.diffView.View())
	}
	if m.runOpen {
		return tea.NewView(m.runView.View())
	}
	switch m.modal {
	case modalPause:
		base = spliceOverlay(base, renderPauseModal(m.pauseModalView()), 8)
	case modalMute:
		base = spliceOverlay(base, renderMuteModal(m.muteModalView()), 8)
	case modalFilter:
		base = spliceOverlay(base, m.filterModalView(), 8)
	case modalDestroy:
		base = spliceOverlay(base, m.destroyConfirmView(), 6)
	}
	if m.flash != "" {
		base = spliceOverlay(base, join(box(1, 56, []string{m.flash})), max(0, m.height-4))
	}
	return tea.NewView(base)
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

// renderPoliciesScreen renders the plan-policies surface, or its error state.
func (m Model) renderPoliciesScreen() string {
	if m.policiesErr != nil {
		return styleErr.Render("failed to load plan policies: " + m.policiesErr.Error())
	}
	return renderPoliciesScreen(m.policiesModel())
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

// renderSecretsScreen renders the read-only secrets surface, or its error state.
func (m Model) renderSecretsScreen() string {
	if m.secretsErr != nil {
		return styleErr.Render("failed to load secrets: " + m.secretsErr.Error())
	}
	return renderSecretsScreen(m.secretsModel())
}

// renderRunnersScreen renders the runner registry surface, or its error state.
func (m Model) renderRunnersScreen() string {
	if m.runnersErr != nil {
		return styleErr.Render("failed to load runners: " + m.runnersErr.Error())
	}
	return renderRunnersScreen(m.runnersModel())
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
		return styleDim.Render("no jobs yet — they appear when a schedule or alert fires")
	}
	return m.jobsTable.View()
}

func (m Model) renderBackups() string {
	if m.backupsErr != nil {
		return styleErr.Render("failed to load backup targets: " + m.backupsErr.Error())
	}
	if len(m.targets) == 0 {
		return styleDim.Render("no backups yet — exocomp backup add")
	}
	return m.backupsTable.View()
}

func (m Model) renderWorkspaces() string {
	if m.workspacesErr != nil {
		return styleErr.Render("failed to load workspaces: " + m.workspacesErr.Error())
	}
	if len(m.workspaces) == 0 {
		return styleDim.Render("no workspaces yet — exocomp workspace add ./infra")
	}
	return m.workspacesTable.View()
}

func (m Model) renderSchedules() string {
	if m.schedulesErr != nil {
		return styleErr.Render("failed to load schedules: " + m.schedulesErr.Error())
	}
	if len(m.schedules) == 0 {
		return styleDim.Render("no schedules yet — exocomp schedule add")
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
	case m.active == screenJobs:
		return styleHelp.Render("[enter] tail  [f] follow  [c] cancel  [/] filter  [?] help")
	case m.active == screenBackups:
		return styleHelp.Render("[b] run now  [enter] last runs  [j/k] move  [?] help")
	case m.active == screenWorkspaces:
		return styleHelp.Render("[enter] detail  [l] plan now  [j/k] move  [?] help")
	case m.active == screenSchedules:
		return styleHelp.Render("[space] on/off  [x] run now  [j/k] move  [?] help")
	default:
		return styleHelp.Render("[?] help")
	}
}
