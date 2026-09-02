package wire

import (
	"bytes"
	"encoding/json"
)

// HasSensitivePlan reports whether a plan's *_sensitive tree marks anything
// secret. It is the ONE classifier every face uses (web, TUI, API, CLI): a
// coarse check on purpose — any true anywhere means the consumer shows
// [redacted · secret] rather than a value the web UI would not. It moved here
// from internal/web so there is no sibling fork to drift (the review's
// standing pattern).
func HasSensitivePlan(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return bytes.Contains(raw, []byte("true"))
}
