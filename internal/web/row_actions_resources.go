package web

// glyphRoute is the checks netgroup slot's 13px location glyph (SPEC §5.1):
// three nodes on two links — the designer's sources/route path verbatim. It
// lives beside the row-action glyphs because it is the same per-verb fixed
// shape language; the checks template renders it, never the row actions.
const glyphRoute = "M6 8.2v7.6M18 9.2c0 4.8-5.5 4.3-9.3 6.9"

// glyphInfo is the rules row's explain affordance (SPEC §5.3): a lowercase i
// in a circle — the "explain last evaluation" glyph that expands the full CEL
// expression inline. The designer's set has no explain verb, so it is drawn to
// the same stroke contract (24 viewBox, stroke 1.8, round caps).
const glyphInfo = "M12 4a8 8 0 1 1 0 0zM12 9v6M12 5.5v2"

// scheduleRowActions is the row-action set for one schedule: "Run now",
// Disable or Enable as the verb glyph (the state toggles), with Open (the spec
// modal) and Remove (confirmed, last) in the chevron menu.
func scheduleRowActions(sc scheduleRow) []rowAction {
	acts := []rowAction{
		{
			Name: "Run now", Path: glyphRun,
			Form:    "/schedules/" + sc.Name + "/run",
			Refusal: sc.Refusal,
		},
	}
	if sc.Enabled {
		acts = append(acts, rowAction{
			Name: "Disable", Path: glyphDisable,
			Form: "/schedules/" + sc.Name + "/disable",
			// Refusal is the row's one refused verb (forms/SPEC.md): rendered
			// as the ▲ status line by rowActions, never a flash.
			Refusal: sc.Refusal,
		})
	} else {
		acts = append(acts, rowAction{
			Name: "Enable", Path: glyphEnable,
			Form:    "/schedules/" + sc.Name + "/enable",
			Refusal: sc.Refusal,
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
			// Refusal is the row's one refused verb (forms/SPEC.md): rendered
			// as the ▲ status line by rowActions, never a flash.
			Refusal: ck.Refusal,
		})
	} else {
		acts = append(acts, rowAction{
			Name: "Enable", Path: glyphEnable,
			Form:    "/checks/" + ck.Name + "/enable",
			Refusal: ck.Refusal,
		})
	}
	acts = append(acts,
		rowAction{
			Name: "Open", Path: glyphOpen, Menu: true,
			Href: urlWithBase(ck.Base, "/events?source=check"),
		},
		rowAction{
			Name: "Remove", Path: glyphRemove, Menu: true,
			Form:    "/checks/" + ck.Name + "/rm",
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
			Confirm: "Remove netgroup " + ng.Name + "?",
			// Refusal is the row's one refused verb (forms/SPEC.md): rendered
			// as the ▲ status line by rowActions, never a flash.
			Refusal: ng.Refusal,
		},
	}
}
