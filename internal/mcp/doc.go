// Package mcp is the third programmatic face (agent-interface.md §3.5): an MCP
// server that lets an agent harness drive exocomp as tools and resources over
// HTTP, through the per-message seam (HandleMessage) internal/web/mcp_http.go
// mounts POST /mcp on. It is a thin adapter, not a second core — every tool
// call and resource read routes to the SAME internal/service verbs and
// internal/store reads the CLI and the HTTP API use, and marshals the SAME
// internal/wire envelope, so a resource's MCP bytes are identical to its CLI
// --output json and its /api/v1 bytes.
//
// This is deliberately the ONE agent-facing MCP surface: no bespoke stdio
// server runs at the local operator's ambient authority. A stdio-only MCP
// client bridges to POST /mcp with an off-the-shelf http-to-stdio wrapper
// (docs/operations/mcp.md); an agent working the local filesystem drives
// exocomp directly through the agent-aware CLI (--output json, exit codes).
//
// Invariant 5 holds here exactly as it does for the HTTP write API: a write
// tool assembles no queue.Spec of its own and never shells out — it calls the
// service verb, which routes through the one queue and the approval gate. This
// face supplies a PURE-ENQUEUE Enqueuer (queue the row, return its id), so a
// running daemon claims and runs it; this process only enqueues.
//
// Auth model: the TRANSPORT authenticates and this package authorizes. Every
// dispatch takes the Caller the transport resolved (TokenCaller, built from an
// Decision 0009 bearer token), and both the advertised catalog and the per-call
// check derive from it, so a readonly token is shown only read tools and a
// revoked or downgraded token stops working on its next call.
//
// Values-free by construction, following every other face: secret tools return
// metadata only (wire.SecretView has no field a ciphertext could occupy), drift
// carries resource addresses/actions/attribute keys but never values, and no
// tool ever returns a secret plaintext or an approval decision link — approve
// and reject are scoped tools that call reactive.Decide server-side instead.
package mcp
