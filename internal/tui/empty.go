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

// emptyWorkspacesBlock is the [3] Workspaces first-day block: the message and
// the command that changes the state, verbatim from
// docs/design/reference/tui/copy.txt (empty states) and the pinned frame
// frames/empty-workspaces-80x24.txt. The live screen renders this block inside
// its own chrome; renderEmptyWorkspaces wraps it in the reference frame.
func emptyWorkspacesBlock() []string {
	return []string{
		indent(31, "no workspaces yet"),
		indent(7, "terraform dirs exocomp watches for drift and plans on your behalf"),
		"",
		indent(17, "add the first:  exocomp workspace add ./infra"),
	}
}

// renderEmptyWorkspaces is the [3] Workspaces empty state as the golden frame
// golden_test.go pins, clock and all. The live screen shares emptyWorkspacesBlock
// through renderWorkspaces (view.go).
func renderEmptyWorkspaces() string {
	return renderEmpty("workspaces · 0", "14:03 UTC+11", emptyWorkspacesBlock(), "[0-9] [s] [u] screens")
}

func indent(n int, s string) string { return fmt.Sprintf("%*s%s", n, "", s) }
