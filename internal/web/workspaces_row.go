package web

// workspaceRowActions is the v0.6 row-action set for one workspace row: Plan
// and Apply verb glyphs for runs-in-server workspaces, and a Backend-config
// menu item that opens the workspace detail page (where the exact backend
// block lives). A state-only workspace has no Plan/Apply — its row shows a
// Backend glyph (links the same detail page) plus the chevron menu. Apply
// appears only while a SAVED plan exists (SPEC §5: one apply per workspace,
// and the list never offers an apply the detail tray would refuse). The row
// itself opens the detail page (data-href).
func workspaceRowActions(wsr workspaceWriteRow) []rowAction {
	acts := []rowAction{}
	if !wsr.StateOnly() {
		acts = append(acts, rowAction{
			Name: "Plan", Path: glyphPlan,
			Form: "/workspaces/" + wsr.Name + "/plan",
		})
		if len(wsr.Plans) > 0 {
			acts = append(acts, rowAction{
				Name: "Apply", Path: glyphApply,
				Form:    "/workspaces/" + wsr.Name + "/apply",
				Hidden:  map[string]string{"confirmed": "1"},
				Confirm: "Apply " + wsr.Name + "?",
			})
		}
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
