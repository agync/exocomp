package tui

import (
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// renderEvents formats a job's event tail as one line per event: a dim
// timestamp, the level, and the message. Error-level lines render in the
// failure color so they stand out while scrolling.
func renderEvents(events []store.JobEvent) string {
	if len(events) == 0 {
		return styleDim.Render("no events yet")
	}

	lines := make([]string, len(events))
	for i, e := range events {
		ts := styleDim.Render(e.Ts.Local().Format("15:04:05"))
		line := ts + "  " + "[" + e.Level + "] " + e.Msg
		if e.Level == store.LevelError {
			line = ts + "  " + styleErr.Render("["+e.Level+"] "+e.Msg)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
