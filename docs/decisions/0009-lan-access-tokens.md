---
type: Decision
title: "Decision 0009 — LAN access: bearer tokens minted by local auth"
description: "LAN listeners require locally-minted bearer tokens, with TLS on by default."
status: stable
tags: [decision, security, auth]
---

# Decision 0009 — LAN access: bearer tokens minted by local auth

Status: accepted · 2026-08-22

## Context

The web UI and phone approval surface must be reachable over the LAN, not only via
loopback or a tailnet. Passwords are banned by posture; user accounts and SSO are
out of scope by contract. The authenticated local surface (filesystem-permission
auth on the data dir, via CLI/TUI on the host) already exists and is the natural
root of trust for minting network credentials.

## Decision

1. **Any non-loopback listener requires bearer authentication.** `exocomp serve`
   refuses to bind a non-loopback address unless at least one active token exists;
   the loopback state backend and `healthz` are unchanged.
2. **Tokens are minted and managed only via local auth**: `exocomp token new
   <name> [--scope operator|readonly|approve] [--expires <dur>]` prints the token
   exactly once (kit discipline); `token list` shows name, scope, expiry,
   last-used, and a non-secret prefix; `token revoke <name>` is immediate. At
   rest: argon2id hashes in the existing secret store; lifecycle columns
   (expiry, last-used, revocation). Minting requires the data dir — that is the
   "local auth".
3. **Browser sessions bootstrap from a token**: presented once, exchanged for an
   HttpOnly, SameSite=Strict session cookie bound to the token row — revoking the
   token invalidates its sessions. Approval callback links remain single-use HMAC
   capabilities and never double as login.
4. **TLS on LAN by default**: the listener generates a self-signed certificate
   (key 0600 in the data dir) on first bind and prints its fingerprint alongside
   the token (and in `doctor`) for trust-on-first-use pinning. `--insecure-http`
   exists, is loud at startup and in the UI footer, and is never the documented
   path. Tailscale Serve / Caddy+ACME remain the recommended fronts where
   available; this record covers the bare-LAN case.
5. Audit rows record the token name behind every session action (transport
   `web`), preserving the resolved-identity guarantee.

## Consequences

The phone surface works on a bare LAN with two artifacts the operator controls
end to end (a token and a pinned fingerprint), both revocable and both minted
from the box. WebAuthn/passkey enrollment stays a possible future refinement on
top of token-bootstrapped sessions, not a v1 requirement. The v1.0 gate gains:
non-loopback serve without a token refuses; wrong/revoked token rejected; session
dies with its token; insecure mode warns.
