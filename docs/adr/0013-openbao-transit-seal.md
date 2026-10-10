---
type: Decision
title: "ADR 0013 — OpenBao Transit as an optional external seal for the DEK"
description: "OpenBao Transit as an optional additive seal for the DEK, keeping the age keyring recovery path and treating unreachable as an explicit error."
status: stable
tags: [decision, secrets, backup]
---

# ADR 0013 — OpenBao Transit as an optional external seal for the DEK

## Status

Accepted · 2026-08-23. Extends ADR 0003: the key-provider seam it named ("an external KMS
or OpenBao Transit key can substitute at bootstrap") is now realised as an *additive*
unseal path, not a substitution.

## Context

ADR 0003 wraps one 32-byte DEK to N age recipients. The daemon opens the store by
unwrapping the DEK with the host identity (`host.key`), whose at-rest protection is a
systemd credential, a scrypt passphrase, or — insecurely — plaintext in the data dir.

Operators who already run OpenBao want the auto-unseal pattern: keep the DEK's wrapping
key in OpenBao, so a stolen data volume never contains what unwraps the DEK, and the daemon
unseals over the network at boot rather than from a key on the same disk it is protecting.

The hard constraint is disaster recovery (invariant 7): recovery must never depend on a
vendor or on a reachable service. So Transit cannot *replace* the age keyring — it can only
*add* a second unseal path for the running daemon.

## Decision

**Additive, optional, backward-compatible.** When no seal is configured, the on-disk layout
and `Open()` are byte-identical to before: `OpenWithSeal(dir, nil)` is exactly `Open(dir)`.
A regression test pins this (`TestUnconfiguredPathUnchanged`).

**A new file beside `keyring.age`.** When configured, the DEK is wrapped with an OpenBao
Transit key (`transit/encrypt`) and the resulting `vault:v1:…` ciphertext is stored as
`keyring.transit`. `keyring.age` is left untouched — it still wraps the same DEK to the age
recipients. The two files wrap the same DEK by two independent means.

**Open path when configured.** `OpenWithSeal` reads `keyring.transit`, calls
`transit/decrypt`, and gets the DEK back with **no host key required** — the auto-unseal
value. `keyring.age`, the recipients file, and the version file are still read (for
`KeyringAge`, backups, and metadata); the host identity is loaded only if present.

**Config sources, token never at rest.** Address, token, and transit key name come from the
environment (`EXOCOMP_SEAL_TRANSIT_ADDRESS`, `_KEY`, `_TOKEN` or `_TOKEN_FILE`, optional
`_NAMESPACE`). The token is read from the environment or a file outside the data dir and is
**never written into the data dir** — only the transit ciphertext is. The transit key
itself never leaves OpenBao.

**No new dependency.** The Transit API is two POST endpoints and a bearer header; a hand
client (`internal/secrets/openbao.go`) speaks it rather than pulling in the Vault SDK.

## The recovery story (explicit)

The age recovery recipient opens the store **without OpenBao**, exactly as before:
`age -d -i recovery.key keyring.age` yields the DEK, and `exocomp kit derive` reproduces
every subkey offline. Transit is an *additional* unseal path for the daemon, never the only
one. Losing OpenBao loses auto-unseal, not the data: the operator either restores OpenBao,
or opens with the host key (unset the seal env), or recovers from the printed card. The
monthly DR drill (ADR 0003) still runs from kit contents alone, with no OpenBao present.

## Failure mode

A configured-but-unreachable OpenBao is an **explicit error** from `Open`, never a silent
fallback to the host-key path — a silent fallback would let a network outage quietly
downgrade the seal an operator chose. `TestUnreachableOpenBaoIsExplicit` proves the error
surfaces even when a valid `host.key` that *would* open the store is present. Reverting to
the host key is a deliberate `exocomp seal disable` (or unsetting the env), not something a
dropped connection does.

## Consequences

- The DEK-zeroed-by-`Close` invariant is unchanged: the transit path lands the DEK in the
  same single `[]byte` the age path does, mlocked and zeroed identically. A too-short
  decrypt result is cleared and rejected.
- Nothing new is logged: the token and the DEK plaintext never reach a log line, a job
  event, or an error string (the transit error names the address and OpenBao's own error
  body, which carries no secret). `TestNoSecretsLeak` asserts it.
- `exocomp seal enable|disable|status` manage the blob; `serve` sources the seal from the
  environment and reports the posture at boot.
- OpenBao can now read the DEK it wraps. This is the same trust already extended to the host
  key's at-rest tier: it moves the wrapping key off the data volume onto OpenBao. It does
  not defend against root on the box, and OpenBao's own seal/audit posture becomes part of
  the operator's threat model.
- Rotating the transit key on OpenBao's side is handled by re-running `seal enable`, which
  re-wraps the current DEK; the old `keyring.transit` is overwritten.

See ADR 0003 for the envelope this extends, and `docs/security/threat-model.md` for the
seal's place in the model.
