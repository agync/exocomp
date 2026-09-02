package tui

import (
	"fmt"
	"image/color"
	"testing"
)

// colorHex reads a lipgloss color back to its #RRGGBB form through the
// image/color interface every palette entry satisfies.
func colorHex(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
}

// TestThemePinsDesignColors locks the design color table (handoff/tui/
// colors.txt v0.2) against drift: the rules, body, and selected-row colors the
// render surface draws from must keep their design hexes.
func TestThemePinsDesignColors(t *testing.T) {
	for _, c := range []struct {
		name string
		got  color.Color
		want string
	}{
		{"colorRules", colorRules, "#4A5254"},
		{"colorBody", colorBody, "#D6DBDD"},
		{"colorSelectedRow", colorSelectedRow, "#1D2426"},
	} {
		if got := colorHex(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestHRuleKeepsNoColorForm proves hrule's color is a hint layered over the
// structural rule: strip the styling and the full run of ─ characters must
// survive, so the rule still reads with no color (the table's no-color
// contract).
func TestHRuleKeepsNoColorForm(t *testing.T) {
	plain := stripANSI(hrule(8))
	if plain != "────────" {
		t.Errorf("hrule plain form = %q, want 8 rule characters", plain)
	}
}
