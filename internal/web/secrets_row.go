package web

// secretRowActions is the v0.6 row-action set for one secret: the change
// (Rotate/Replace) and Remove verb glyphs, and — when the secret is bound — a
// Bindings menu item that opens the read-only bindings dialog. The change verb
// is named for the row's source; it opens the reauth dialog (the
// value/generate/confirm-token form). Remove POSTs with the confirmed flag (the
// shared confirm dialog is the safety). The change glyph is only offered when
// sealing is available (a web-only surface holds no envelope).
func secretRowActions(sc secretRow, sealAvailable bool) []rowAction {
	acts := []rowAction{}
	if sealAvailable {
		acts = append(acts, rowAction{
			Name: sc.changeVerb(), Path: glyphRotate,
			Modal: "secret-rotate-" + sc.Name,
		})
	}
	acts = append(acts, rowAction{
		Name: "Remove", Path: glyphRemove,
		Form:    "/secrets/" + sc.Name + "/rm",
		Confirm: "Delete secret " + sc.Name + "?",
		// Refusal is the rm verb's refusal (forms/SPEC.md): rendered as the ▲
		// status line by rowActions, never a flash. The Rotate modal carries
		// its own refusal inside the dialog, so it never reads this field.
		Refusal: sc.Refusal,
	})
	if len(sc.Bindings) > 0 {
		acts = append(acts, rowAction{
			Name: "Bindings", Path: glyphBindings, Menu: true,
			Modal: "secret-bindings-" + sc.Name,
			Count: itoa(int64(len(sc.Bindings))),
		})
	}
	return acts
}
