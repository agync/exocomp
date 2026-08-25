package tui

import "charm.land/lipgloss/v2"

// Palette mirrors the design's TUI color table (handoff/tui/colors.txt v0.2):
// color is a hint, never the message — every state also reads as text, so these
// only tint word forms that stand on their own. Truecolor values; lipgloss
// degrades them to the ANSI-16 fallbacks the table pins (magenta 13, green 2,
// yellow 3, red 9, bright white 15, bright black 8; body ink is the terminal's
// own default fg).
var (
	colorAccent  = lipgloss.Color("#BE7BEB") // keys, links, active tab
	colorOk      = lipgloss.Color("#4FB86B") // succeeded, running, in sync
	colorWait    = lipgloss.Color("#E3A008") // awaiting, stale, drift, waiting
	colorFail    = lipgloss.Color("#F0645A") // failed, timed_out
	colorHeading = lipgloss.Color("#ECEFF0") // headings, bold ink
	colorDim     = lipgloss.Color("#8A9296") // meta, disabled, queued, cancelled

	styleTitle       = lipgloss.NewStyle().Bold(true).Foreground(colorHeading)
	styleTabActive   = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleTabInactive = lipgloss.NewStyle().Foreground(colorDim)
	styleHelp        = lipgloss.NewStyle().Foreground(colorDim)
	styleErr         = lipgloss.NewStyle().Foreground(colorFail)
	styleSuccess     = lipgloss.NewStyle().Foreground(colorOk)
	styleWait        = lipgloss.NewStyle().Foreground(colorWait)
	styleDim         = lipgloss.NewStyle().Foreground(colorDim)
)

// renderState colors a job or backup run state per the design's semantic table:
// succeeded and running are ok-green (running carries its own word, and the
// design's breathing dot, as the signal), terminal failures are red,
// awaiting_approval is wait-yellow, and the inactive rest (queued, cancelled,
// interrupted) is dim. Color is a hint; the state word is the message.
func renderState(state string) string {
	switch state {
	case "succeeded", "running":
		return styleSuccess.Render(state)
	case "failed", "timed_out":
		return styleErr.Render(state)
	case "awaiting_approval":
		return styleWait.Render(state)
	default:
		return styleDim.Render(state)
	}
}
