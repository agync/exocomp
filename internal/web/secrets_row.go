package web

// secretRowActions is the v0.6 row-action set for one secret: the Rotate and
// Remove verb glyphs, and — when the secret is bound — a Bindings menu item
// that opens the read-only bindings dialog. Rotate opens the reauth dialog
// (the value/generate/confirm-token form); Remove POSTs with the confirmed
// flag (the shared confirm dialog is the safety). The Rotate glyph is only
// offered when sealing is available (a web-only surface holds no envelope).
func secretRowActions(sc secretRow, sealAvailable bool) []rowAction {
	acts := []rowAction{}
	if sealAvailable {
		acts = append(acts, rowAction{
			Name: "Rotate", Path: glyphRotate,
			Modal: "secret-rotate-" + sc.Name,
		})
	}
	acts = append(acts, rowAction{
		Name: "Remove", Path: glyphRemove,
		Form:    "/secrets/" + sc.Name + "/rm",
		Hidden:  map[string]string{"confirmed": "1"},
		Confirm: "Delete secret " + sc.Name + "?",
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
