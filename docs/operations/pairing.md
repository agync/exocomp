---
type: Guide
title: "Device pairing by QR code"
description: "The phone companion pairing payload a QR code carries, rendered on the token result page and by the token command, plus setting the public URL."
status: stable
tags: [operations, mobile, pairing]
---

# Device pairing (QR)

Pairing hands a phone companion everything it needs to reach and authenticate
against this control plane in one scan: the base URL, a freshly minted access
token, the certificate fingerprint to pin, the token's scope, and a device
name. A QR carries that as a small JSON payload; the phone decodes it and
connects, pinning the fingerprint trust-on-first-use on the first request.

Pairing is minting a token with a picture attached — it adds no new credential
or trust path. The token in the payload is the same one-time secret `token new`
prints; the fingerprint is the same one [LAN access](lan-access.md) pins.

## The payload

The QR encodes this JSON (schema version `v`, currently `1`):

```json
{
  "v": 1,
  "url": "https://exocomp.example.ts.net",
  "token": "exo_42_x3Y…",
  "fingerprint": "AB:CD:…",
  "scope": "operator",
  "name": "pixel-8"
}
```

| Field | Meaning |
|---|---|
| `v` | Payload schema version; a scanner keys its parsing off it. |
| `url` | Base URL the phone dials — the operator-set public URL, else the browser's own origin. |
| `token` | The full one-time access token (`exo_<id>_<secret>`). Holding the QR is holding the token. |
| `fingerprint` | Certificate SHA-256 to trust-on-first-use pin. Omitted only when it cannot be resolved (the phone still pins on first connect). |
| `scope` | The token's scope: `operator`, `readonly`, or `approve`. |
| `name` | The device name the token was minted under. |

The payload is defined once in `internal/pairing` and rendered two ways: an
inline SVG on the web token-result page and a half-block QR in the terminal.

## From the browser

On the tokens page, the "Mint a token" action mints a token and shows the
secret once, a copy control, and the pairing QR. Leave the token name blank to
auto-generate a unique `device-<hex>` name; pick a scope (operator is the
default); confirm with your own access token. A rotated token shows the QR too.

The QR's `url` is the operator-set public URL when configured, else the address
the browser reached the page on, so pairing works before a public URL is set.

## From the CLI

```
exocomp token new pixel-8 --scope operator
```

`token new` prints the token once, the fingerprint to pin, and — by default — a
scannable terminal QR with the same payload (`--qr=false` to omit). The `url`
comes from `daemon.public_url`; when unset the QR carries a clear placeholder
and the output says to set it.

## The public URL

`url` resolves from the `daemon.public_url` setting. Set it with
`exocomp serve --public-url <url>`, or on the **System** page's Public URL
control (operator-gated). When unset, the browser flow falls back to the
request's own scheme and host; the CLI flow emits a placeholder.

## Handling

The payload holds a live credential. It is never logged, persisted, or flashed
through a redirect — it exists only on the operator's screen for the one render,
the same discipline as the one-time secret it carries.

Related: [LAN access](lan-access.md) · [Running the daemon](serve.md).
