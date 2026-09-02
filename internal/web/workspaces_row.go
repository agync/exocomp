package web

// workspaceRowActions is the v0.6 row-action set for one workspace row: Plan
// and Apply verb glyphs for runs-in-server workspaces, and a Backend-config
// menu item that opens the workspace detail page (where the exact backend
// block lives). A state-only workspace has no Plan/Apply, so its row shows
// only the chevron menu. The row itself opens the detail page (data-href).
func workspaceRowActions(wsr workspaceWriteRow) []rowAction {
	acts := []rowAction{}
	if !wsr.StateOnly() {
		acts = append(acts, rowAction{
			Name: "Plan", Path: glyphPlan,
			Form: "/workspaces/" + wsr.Name + "/plan",
		})
		acts = append(acts, rowAction{
			Name: "Apply", Path: glyphApply,
			Form:    "/workspaces/" + wsr.Name + "/apply",
			Hidden:  map[string]string{"confirmed": "1"},
			Confirm: "Apply " + wsr.Name + "?",
		})
	}
	acts = append(acts, rowAction{
		Name: "Backend config", Path: glyphOpen, Menu: true,
		Href: "/workspaces/" + wsr.Name,
	})
	return acts
}
