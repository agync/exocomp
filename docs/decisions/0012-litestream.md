---
type: Decision
title: "Decision 0012 — Litestream as an optional fast-RPO sidecar"
description: "Litestream as an optional fast-RPO sidecar replicating the database WAL, with restic staying authoritative and an unencrypted remote refused without an acknowledgement."
status: stable
tags: [decision, backup, replication]
---

# Decision 0012 — Litestream as an optional fast-RPO sidecar

## Status

Accepted · 2026-08-22 — complements the restic DR chain in
[operations/disaster-recovery.md](../operations/disaster-recovery.md); does not
supersede it. Amended 2026-10-22 to ship the binary in the image (see the addendum).

## Context

The default DR chain is three restic legs plus a 15-minute DB-only fast path
(disaster-recovery.md). That fast path is a `VACUUM INTO` and a `restic backup` of
`exocomp.db` on a timer: cheap, encrypted, drill-covered — and bounded below by its
interval. An operator who cannot accept losing up to 15 minutes of job history wants
a tighter RPO than a timer can give, without a second heavy service inside the
container.

Litestream ships a SQLite database's WAL frames to a replica target continuously,
giving seconds-level RPO. An earlier design draft deferred it; review flagged the
one property that constrains how it can be adopted here: **Litestream does not
encrypt.** It streams WAL frames to the target as
it reads them. That is safe to a LAN-local disk or an already-encrypted store, and
unsafe to a plaintext bucket over the open internet.

Two rules from the existing architecture bound the decision:

- **restic stays authoritative** (invariant 7; disaster-recovery.md). DR must not
  depend on a vendor or on this project's code, and the encrypted, drill-covered
  chain a bare host recovers from is restic. Litestream cannot become the thing the
  recovery card derives, because its replica is not encrypted and its restore is not
  the manual `age`/`openssl`/`restic` path the card guarantees.
- **no second heavy service inside the daemon** (Decision 0001). The thin core owns state,
  sequencing, keys, and recovery, and shells out to pinned tools for everything else.
  A backup engine compiled into `exocompd` would violate the one hard rule of Decision
  0001 as surely as an embedded Terraform executor would.

## Decision

**Adopt Litestream as an OPTIONAL sidecar that replicates `exocomp.db`'s WAL for
seconds-level RPO — the fast-RPO complement to restic, never its replacement.**

- **Sidecar, not embedded.** Litestream runs as its own process beside `exocompd`,
  like restic and tofu are their own processes. `exocompd` does not link it, compile
  it in, or spawn it from inside the daemon. It **generates** the `litestream.yml`
  config (`internal/litestream.GenerateConfig`, deterministic) and **observes** the
  replica's freshness; the operator (or a Quadlet/compose unit) runs the binary. A
  host without the binary degrades honestly: `exocomp litestream status` and
  `exocomp doctor` report it absent and name the one install step, and the
  freshness gauge reads +Inf ("unknown"), never a misleading zero.

- **restic stays the authoritative encrypted DR chain.** Litestream tightens RPO from
  ~15 min to ~1 s for operators who want it; it changes nothing about restore. The
  recovery card, the drill, and the three legs are untouched. Losing 15 minutes of
  job history is not a data-loss event worth a second process for most operators, so
  the sidecar stays off by default.

- **The no-encryption caveat is enforced, not documented.** A replica target must be
  LAN-local (a path, `file://`, an NFS/SMB mount) or an already-encrypted store. A
  remote target (`s3://`, `sftp://`) is **refused** at `litestream target` and at
  config generation unless the operator passes `--ack-unencrypted`, which asserts the
  endpoint is encrypted or the plaintext risk is accepted. The acknowledged config
  carries the warning in its own header, so the caveat is legible in the artifact.

- **Credentials go through the sealed secret plane.** A remote target's credentials
  are never written into the emitted config. Litestream reads them at launch from
  `LITESTREAM_ACCESS_KEY_ID` / `LITESTREAM_SECRET_ACCESS_KEY` (and the SFTP
  equivalents), which the sidecar's launch unit populates from the sealed secret
  plane — the same seam restic's s3 leg uses. The generated config documents this at
  the replica block rather than leaving a plaintext-credential temptation.

## Consequences

- An operator who needs it gets ~1 s RPO by installing one binary and pointing a
  Quadlet/compose unit at the generated config; an operator who does not is unaffected
  and pays for no second process.
- `exocomp doctor` gains a litestream section (binary presence, configured target,
  RPO age) and `/metrics` gains
  `exocomp_litestream_replication_staleness_seconds` — the RPO age, +Inf when unknown
  — beside the other staleness gauges, so a silently stopped sidecar drifts an alert
  the same way a stopped backup leg does.
- Config generation is deterministic and golden-tested, and freshness for a file
  replica is read from the replica's own layout, so the whole feature is unit-tested
  with no litestream binary present; any e2e that shells the binary SKIPs cleanly when
  it is absent, like the tofu/ansible gates.
- The encryption caveat is a build-enforced refusal, not a footnote: a plaintext
  remote target cannot be persisted or rendered without the explicit ack, closing the
  most likely way to leak an unencrypted database off-LAN.
- Litestream is a new optional entry in the bill of materials (already listed as an
  optional sidecar); it is Apache-2.0 and crosses a process boundary, so it raises no
  license-boundary question (bill-of-materials.md).
- Restore stays restic's job. The `litestream restore` fast path
  (`internal/litestream.RestoreArgs`) exists for catching a replica up, but the
  authoritative "control host is gone" runbook is unchanged: it derives the restic
  password from the DEK and restores the encrypted chain.

## Addendum of 2026-10-22, the binary in the image

The base image carries `litestream`, pinned and sha256-checked like restic and tofu
([bill of materials](../sbom/bill-of-materials.md)). `exocomp restore --from-litestream`
shells it, and without it in the image that command refused in the container. A sidecar
could write a replica that the container could not read back.

The sidecar decision stands. `exocompd` does not run the replication process, replication
stays off until the operator starts `litestream replicate` against the generated config,
and restic stays the authoritative chain. What changes is the install step. On the shipped
image there is none. On a host install outside the image the binary is still the
operator's to add, which `exocomp doctor` names. The binary adds about 39 MB to the image.
