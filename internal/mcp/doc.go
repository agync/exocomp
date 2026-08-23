// Package mcp is the third programmatic face (agent-interface.md §3.5): an MCP
// server that lets an agent harness drive exocomp as tools and resources, over
// stdio (Serve) or over a transport that mounts the per-message seam
// (HandleMessage). It is a thin adapter, not a second core — every tool call and
// resource read routes to the SAME internal/service verbs and internal/store
// reads the CLI and the HTTP API use, and marshals the SAME internal/wire
// envelope, so a resource's MCP bytes are identical to its CLI --output json
// and its /api/v1 bytes.
//
// Invariant 5 holds here exactly as it does for the HTTP write API: a write
// tool assembles no queue.Spec of its own and never shells out — it calls the
// service verb, which routes through the one queue and the approval gate. This
// face supplies a PURE-ENQUEUE Enqueuer (queue the row, return its id), so a
// running daemon claims and runs it; this process only enqueues.
//
// Auth model: the TRANSPORT authenticates and this package authorizes. Every
// dispatch takes the Caller the transport resolved, and both the advertised
// catalog and the per-call check derive from it, so a readonly token is shown
// only read tools and a revoked or downgraded token stops working on its next
// call. Stdio has no credential to resolve, so it runs as LocalCaller — the
// local operator's authority, the same trust boundary a local `exocomp`
// invocation has — and deliberately without the approve capability.
//
// Values-free by construction, following every other face: secret tools return
// metadata only (wire.SecretView has no field a ciphertext could occupy), drift
// carries resource addresses/actions/attribute keys but never values, and no
// tool ever returns a secret plaintext or an approval decision link — approve
// and reject are scoped tools that call reactive.Decide server-side instead.
package mcp
