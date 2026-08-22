package tui

import "fmt"

// renderEmpty draws a first-day empty state: the screen's title and rule, a
// healthy-and-expectant message block, and the help bar. The voice names the
// command that changes the state rather than apologising for the emptiness
// (docs/design/reference/tui/copy.txt, empty states).
func renderEmpty(title, clock string, block []string, keysLeft string) string {
	const w = 80
	head := []string{lr(title, clock, w), hrule(w)}
	body := make([]string, 0, 8+len(block))
	for i := 0; i < 8; i++ {
		body = append(body, "")
	}
	body = append(body, block...)
	footer := []string{hrule(w), lr(keysLeft, helpRight, w)}
	return frame(24, head, body, footer)
}

// renderEmptyWorkspaces is the [3] Workspaces empty state.
func renderEmptyWorkspaces() string {
	block := []string{
		indent(31, "no workspaces yet"),
		indent(7, "terraform dirs exocomp watches for drift and plans on your behalf"),
		"",
		indent(17, "add the first:  exocomp workspace add ./infra"),
	}
	return renderEmpty("workspaces · 0", "14:03 UTC+11", block, "[0-6] screens  [q] quit")
}

func indent(n int, s string) string { return fmt.Sprintf("%*s%s", n, "", s) }
