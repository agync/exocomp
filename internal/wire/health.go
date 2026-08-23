package wire

// Health status is a small fixed enum so a weak model branches on it directly
// instead of parsing prose: ok, warn, and fail are self-explanatory; unknown
// covers a subsystem doctor could not observe (no daemon running, no database
// yet, an optional sidecar never configured) — a state, not a fault.
const (
	HealthOK      = "ok"
	HealthWarn    = "warn"
	HealthFail    = "fail"
	HealthUnknown = "unknown"
)

// HealthEntry is one subsystem's health line: doctor's per-subsystem read,
// stripped to what an agent needs to branch — a stable status and a terse,
// values-free detail. Detail mirrors only what the text report already prints
// (invariant 4): identifiers and states, never a secret, a scrape token, or a
// raw metrics/subprocess body, and no volatile field (a relative age, a
// "now") that would make repeated polls diff for no reason.
type HealthEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// HealthView is the doctor summary: subsystem entries in a fixed,
// deterministically-ordered sequence — declaration order, the order doctor
// probes them in, never map order.
type HealthView []HealthEntry
