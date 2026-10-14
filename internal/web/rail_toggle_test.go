package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRailTogglePanelGlyph is the panel-glyph guard: the collapse control stops
// wearing the back chevron and names the panel it switches. The id, the
// handler, the aria/title, and the collapsed flyout label stay put.
func TestRailTogglePanelGlyph(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system")
	if strings.Contains(page, "railchev") || strings.Contains(page, "M10 3L5 8l5 5") {
		t.Error("the rail toggle must not render the back chevron")
	}
	for _, want := range []string{
		`class="railtoggle" id="railbtn" onclick="exoToggleRail(event)" aria-label="Collapse sidebar" title="Collapse sidebar"`,
		`class="railpanel"`,
		`M5 2.5h6a2.5 2.5 0 0 1 2.5 2.5v6a2.5 2.5 0 0 1-2.5 2.5H5A2.5 2.5 0 0 1 2.5 11V5A2.5 2.5 0 0 1 5 2.5zM6 2.5v11`,
		`<span class="raillabel">Open sidebar</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rail toggle missing %q", want)
		}
	}
}
