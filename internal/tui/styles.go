package tui

import "charm.land/lipgloss/v2"

// Restrained monochrome palette with one accent, used for state text and
// chrome. No emoji, no gradients.
var (
	colorAccent  = lipgloss.Color("39")  // running / active tab
	colorDim     = lipgloss.Color("245") // inactive / queued / cancelled
	colorFg      = lipgloss.Color("252") // default text
	colorSuccess = lipgloss.Color("42")  // succeeded
	colorFail    = lipgloss.Color("203") // failed / timed_out

	styleTitle       = lipgloss.NewStyle().Bold(true).Foreground(colorFg)
	styleTabActive   = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleTabInactive = lipgloss.NewStyle().Foreground(colorDim)
	styleHelp        = lipgloss.NewStyle().Foreground(colorDim)
	styleErr         = lipgloss.NewStyle().Foreground(colorFail)
	styleSuccess     = lipgloss.NewStyle().Foreground(colorSuccess)
	styleAccent      = lipgloss.NewStyle().Foreground(colorAccent)
	styleDim         = lipgloss.NewStyle().Foreground(colorDim)
)

// renderState colors a job or backup run state for inline display within a
// table cell. Terminal failure states are red, success is green, an
// in-progress state is the accent, everything else (queued, cancelled,
// interrupted, awaiting_approval) is dim.
func renderState(state string) string {
	switch state {
	case "succeeded":
		return styleSuccess.Render(state)
	case "failed", "timed_out":
		return styleErr.Render(state)
	case "running":
		return styleAccent.Render(state)
	default:
		return styleDim.Render(state)
	}
}
