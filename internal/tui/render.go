package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// itoa is strconv.Itoa, kept short because the screen renderers use it inline
// in section headers and counts.
func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// The v0.2 TUI screens render as the no-color text forms first: every glyph in
// the golden fixtures is single width, so display width is the rune count, and
// layout is column placement over that. Color is layered on top elsewhere and
// never changes a column, which is what keeps the fixtures the contract
// (docs/design/reference/tui/colors.txt: "color is a hint, never the message").

// rw is the display width of s. Every glyph the fixtures use — ·, ─, →, ●, ▸,
// ✓, ▌, │ — is single width, so the rune count is the column count.
func rw(s string) int { return utf8.RuneCountInString(s) }

// cell places text starting at an absolute column.
type cell struct {
	col  int
	text string
}

// row lays cells out at their absolute columns, padding the gaps with spaces.
// Cells must be given in ascending, non-overlapping column order. Trailing
// space is never added, so a row is exactly as wide as its last cell ends —
// the fixtures carry no trailing whitespace on content lines.
func row(cells ...cell) string {
	var b strings.Builder
	cur := 0
	for _, c := range cells {
		if c.col > cur {
			b.WriteString(strings.Repeat(" ", c.col-cur))
			cur = c.col
		}
		b.WriteString(c.text)
		cur += rw(c.text)
	}
	return b.String()
}

// lr justifies left and right against width: left, then the spaces that push
// right flush to the width. A pair too wide for the width gets a single space
// between them rather than an overlap.
func lr(left, right string, width int) string {
	gap := width - rw(left) - rw(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// hrule is the structural rule spanning the full width.
func hrule(width int) string { return strings.Repeat("─", width) }

// rjust right-justifies s in a field of the given width (used for the age
// column, where "2h", "26h" and "12d" all end on the same column).
func rjust(s string, width int) string {
	if pad := width - rw(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// ljust left-justifies s in a field of the given width.
func ljust(s string, width int) string {
	if pad := width - rw(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// frame assembles a full-height screen: a fixed head (title + rule), the body
// top-aligned, blank lines filling the middle, and a footer pinned to the
// bottom. It is how Home and Approvals reach the terminal's full height with
// the help bar always on the last row.
func frame(height int, head, body, footer []string) string {
	lines := make([]string, 0, height)
	lines = append(lines, head...)
	lines = append(lines, body...)
	for len(lines) < height-len(footer) {
		lines = append(lines, "")
	}
	if len(lines) > height-len(footer) {
		lines = lines[:height-len(footer)]
	}
	lines = append(lines, footer...)
	return strings.Join(lines, "\n") + "\n"
}

// join renders a slice of lines as a fixture: newline-separated, with the
// trailing newline every golden frame carries.
func join(lines []string) string { return strings.Join(lines, "\n") + "\n" }

// box draws a single-line-border panel at the given indent. innerWidth is the
// number of rule characters between the corners; each content line is wrapped
// as "│ text… │" padded to fit. It renders the mute modal and the destroy
// confirmation.
func box(indent, innerWidth int, lines []string) []string {
	pad := strings.Repeat(" ", indent)
	out := make([]string, 0, len(lines)+2)
	out = append(out, pad+"┌"+strings.Repeat("─", innerWidth)+"┐")
	for _, ln := range lines {
		out = append(out, pad+"│ "+ljust(ln, innerWidth-2)+" │")
	}
	out = append(out, pad+"└"+strings.Repeat("─", innerWidth)+"┘")
	return out
}
