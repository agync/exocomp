// Package wire is the presentation-only structured-output contract: the single
// envelope, the schema projections, the typed error taxonomy, and the
// exit-code table that every programmatic face (CLI --output json today; the
// HTTP API and MCP later) marshals through.
//
// It is a leaf of the presentation layer. It imports the domain packages whose
// read-models it projects (store, reactive) but nothing above it — no cobra, no
// net/http, no web server — so the faces that sit above those layers can all
// import wire without a cycle. That one-marshaller discipline is what keeps the
// faces byte-identical under the golden tests.
//
// Two rules make the output cache-stable for a small local model that re-polls
// (docs/design/agent-interface.md §3.6): the bytes are deterministic — struct
// fields marshal in declaration order, map keys sort — and volatile fields (a
// relative age, a "now") are never emitted. Timestamps are fixed RFC3339 UTC.
//
// The projections are values-free (invariant 4, the tofu-drift precedent):
// identifiers, states, keys, and counts travel; attribute values, ciphertext,
// nonces, token hashes, and full stdout never do.
package wire
