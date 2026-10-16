package web

// plural picks the noun for a count. One helper, so the same count never
// renders "1 serials" on one surface and "1 serial" on another — the
// workspace list, the workspace detail tray, the Tofu state tile, and the
// backups snapshot count all read it.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
