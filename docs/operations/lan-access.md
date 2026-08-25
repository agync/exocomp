# LAN access: tokens and the web surface

The browser dashboard and phone approval surface reach the LAN over a bearer-token-gated
TLS listener that is separate from the loopback state backend. The model is
[ADR 0009](../adr/0009-lan-access-tokens.md).

## Two listeners, two auth stories

| Listener | Bind | Auth |
|---|---|---|
| State backend | `127.0.0.1` only | Per-workspace bearer derived from the envelope; never a LAN token |
| Web + API surface | Non-loopback, TLS | A LAN access token, exchanged for a session cookie |

The state backend is the OpenTofu HTTP backend; it stays on loopback and takes no LAN
token. The web surface is the only thing that binds a routable address, and it always
requires a token: a non-loopback bind refuses to start unless at least one active token
exists.

## Minting and managing tokens

Tokens are minted only from the box (the data dir is the root of trust):

```
exocomp token new laptop --scope operator --expires 720h
exocomp token list
exocomp token revoke laptop
```

`token new` prints the token exactly once (kit discipline). `token list` shows name,
scope, expiry, last-used, and a non-secret prefix. Revoking a token is immediate and
invalidates every session it bootstrapped.

## Serving the surface

Run the surface as its own process, or alongside the daemon:

```
exocomp web --addr 0.0.0.0:8443
exocomp serve --web-addr 0.0.0.0:8443
```

With `serve --web-addr`, the daemon runs both listeners in one process sharing one engine,
so an approval decided in a browser releases the job into that daemon's queue. On first
bind the listener generates a self-signed certificate (key `0600` in the data dir) and
prints its SHA-256 fingerprint for trust-on-first-use pinning; the fingerprint also shows
in `doctor`.

A browser presents the token once and receives an HttpOnly, SameSite=Strict session
cookie bound to the token row. `--insecure-http` exists, is loud at startup and in the UI
footer, and is never the documented path — front the surface with Tailscale Serve or
Caddy+ACME where available.

To hand a token to a phone by scanning instead of pasting, see
[Device pairing (QR)](pairing.md).

Related: [Running the daemon](serve.md) · [Reacting to alerts](reactive.md).
