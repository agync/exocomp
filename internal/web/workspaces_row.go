package web

// workspaceRowActions is the v0.6 row-action set for one workspace row: Apply
// and Plan verb glyphs for runs-in-server workspaces, and a Backend-config
// menu item that opens the workspace detail page (where the exact backend
// block lives). A state-only workspace has no Plan/Apply — its row shows a
// Backend glyph (links the same detail page) plus the chevron menu. The
// cluster is [apply]·plan·▾ flush right (SPEC §4): Plan and the chevron hold
// the two right-most slots on every row, and the conditional Apply is prepended
// LEFT only while an applicable plan exists (serial-equal — the detail tray's
// rule), so plan and ▾ never move. Without apply the same two glyphs sit in
// the same two slots, and the TOFU STATE meta reads "no saved plan".
func workspaceRowActions(wsr workspaceWriteRow) []rowAction {
	acts := []rowAction{}
	if !wsr.StateOnly() {
		if wsr.ApplicablePlan() {
			acts = append(acts, rowAction{
				Name: "Apply", Path: glyphApply,
				Form:    "/workspaces/" + wsr.Name + "/apply",
				Hidden:  map[string]string{"confirmed": "1"},
				Confirm: "Apply " + wsr.Name + "?",
			})
		}
		acts = append(acts, rowAction{
			Name: "Plan", Path: glyphPlan, Label: "plan",
			Form: "/workspaces/" + wsr.Name + "/plan",
		})
	} else {
		acts = append(acts, rowAction{
			Name: "Backend", Path: glyphOpen,
			Href: "/workspaces/" + wsr.Name,
		})
	}
	acts = append(acts, rowAction{
		Name: "Backend config", Path: glyphOpen, Menu: true,
		Href: "/workspaces/" + wsr.Name,
	})
	return acts
}
