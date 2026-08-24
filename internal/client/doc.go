// Package client is the typed HTTP client the CLI uses in remote mode to drive a
// remote exocompd over the tailnet (the workstation -> daemon path, agent-interface
// §3.3). It targets the read-only /api/v1 surface (internal/web/api.go), sends
// the ADR-0009 bearer token, and verifies the daemon's self-signed certificate
// against a pinned SHA-256 fingerprint (trust-on-first-use, ADR 0009 §4) — never
// a CA chain. It decodes the shared wire envelope into the SAME wire projection
// types the CLI's --output json emits, so a resource's remote bytes and its
// local bytes are identical, and it surfaces the wire error envelope as a typed
// APIError an agent can branch on by code.
//
// It uses only net/http + crypto/tls: no third-party HTTP framework, and no
// dependency on internal/store or internal/web, so it links into a workstation
// build that has no data dir.
//
// The surface is read.go (the GET routes), write.go (the POST routes — every one
// ENQUEUES, returning identifiers and states, never values), and follow.go (the
// SSE job stream an operator watches a remote run through). A write threads the
// caller's Idempotency-Key, so a retried request dedups to one job rather than
// enqueuing a second.
package client
