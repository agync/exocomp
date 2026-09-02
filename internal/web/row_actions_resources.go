package web

// scheduleRowActions is the v0.6 row-action set for one schedule: Disable or
// Enable as the verb glyph (the state toggles), with Open (the spec modal) and
// Remove (confirmed, last) in the chevron menu. "Run now" is deferred — a
// schedule-fire route + enqueue semantics does not exist yet (noted to the
// designer).
func scheduleRowActions(sc scheduleRow) []rowAction {
	acts := []rowAction{}
	if sc.Enabled {
		acts = append(acts, rowAction{
			Name: "Disable", Path: glyphDisable,
			Form: "/schedules/" + sc.Name + "/disable",
		})
	} else {
		acts = append(acts, rowAction{
			Name: "Enable", Path: glyphEnable,
			Form: "/schedules/" + sc.Name + "/enable",
		})
	}
	acts = append(acts,
		rowAction{
			Name: "Open", Path: glyphOpen, Menu: true,
			Modal: "sched-" + sc.Name,
		},
		rowAction{
			Name: "Remove", Path: glyphRemove, Menu: true,
			Form:    "/schedules/" + sc.Name + "/rm",
			Hidden:  map[string]string{"confirmed": "1"},
			Confirm: "Remove schedule " + sc.Name + "?",
			Danger:  true,
		},
	)
	return acts
}

// checkRowActions is the v0.6 row-action set for one check card: Disable or
// Enable as the verb glyph, with Open (the events view) and Remove (confirmed,
// last) in the chevron menu. Rendered in the card foot (R2/v0.5).
func checkRowActions(ck checkRow) []rowAction {
	acts := []rowAction{}
	if ck.Enabled {
		acts = append(acts, rowAction{
			Name: "Disable", Path: glyphDisable,
			Form: "/checks/" + ck.Name + "/disable",
		})
	} else {
		acts = append(acts, rowAction{
			Name: "Enable", Path: glyphEnable,
			Form: "/checks/" + ck.Name + "/enable",
		})
	}
	acts = append(acts,
		rowAction{
			Name: "Open", Path: glyphOpen, Menu: true,
			Href: "/events?source=check",
		},
		rowAction{
			Name: "Remove", Path: glyphRemove, Menu: true,
			Form:    "/checks/" + ck.Name + "/rm",
			Hidden:  map[string]string{"confirmed": "1"},
			Confirm: "Remove check " + ck.Name + "?",
			Danger:  true,
		},
	)
	return acts
}

// netgroupRowActions is the v0.6 row-action set for one netgroup: a single
// Remove glyph (fewer than three actions → glyphs only, no chevron).
func netgroupRowActions(ng netgroupRow) []rowAction {
	return []rowAction{
		{
			Name: "Remove", Path: glyphRemove,
			Form:    "/netgroups/" + ng.Name + "/rm",
			Hidden:  map[string]string{"confirmed": "1"},
			Confirm: "Remove netgroup " + ng.Name + "?",
		},
	}
}
