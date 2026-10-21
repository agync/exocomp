---
type: Decision
title: "Decision 0005 — Job sandboxing: the `enforcing` tier via nono"
description: "Job sandboxing via nono's enforcing tier, so a job is denied every powerful local socket."
status: stable
tags: [decision, security, sandbox]
---

# Decision 0005 — Job sandboxing: the `enforcing` tier via nono

## Status

Accepted · 2026-08-21

## Context

Jobs execute through an `ExecutorDriver` interface with two tiers designed
([execution](../architecture/execution.md)):

- **`disabled`** — pinned tools in the image, subprocess boundary, per-job tmpfs, separate
  executor UID. The only topology bootstrap and DR require.
- **`container`** — per-job sibling containers, deferred post-v1.

The gap between them is real. Under `disabled`, a playbook — arbitrary code from a git repo —
shares a namespace with the mlocked DEK, the per-job ssh-agent, and the API socket. The
`container` tier closes that, but it costs `exocompd` a mounted podman socket (effectively
host-user power: the sensitive capability moves rather than disappears) and it puts
registry-pulled execution-environment images on the restore path.

## Decision

Adopt **nono** (Apache-2.0, Rust; Landlock on Linux, Seatbelt on macOS, seccomp
user-notification network filtering, unprivileged self-restriction — so it works under
rootless podman) as a middle tier.

**`enforcing` is the default executor on Linux**, secure-by-default: where the kernel
cannot enforce it, `serve` and `doctor` fail closed rather than run the fleet's jobs
unsandboxed (the `opportunistic` tier is the opt-in that degrades loudly instead).
Every job subprocess runs under `nono run --profile job-<kind>` with default-deny
filesystem:

| | Paths |
|---|---|
| Allowed | the job tmpfs, the repo checkout, the artifact dir, tool paths, provider and collection caches |
| Denied | `exocomp.db`, `host.key`, the keyring, other jobs' tmpfs, **the exocomp API socket, and any podman socket** |

Network is scoped per runbook kind: Ansible gets SSH to inventory hosts; OpenTofu gets
provider endpoints plus the `127.0.0.1` state backend.

Amended 2026-10-21: the dependency resolvers reach named package index hosts through
nono's filtering proxy instead of a port grant, because `uv` does not resolve names
through the system resolver. See [job sandboxing](../guide/sandbox.md#egress).

Amended 2026-10-21: a container that mounts the host resolver's socket for name resolution
puts a pathname socket where a job can name it, so the placement argument below does not
hold for that socket. See [containers](../operations/containers.md#resolving-host-names-under-the-sandbox)
and the [threat model](../security/threat-model.md).

Version floor **≥ 0.55.0**, sha256-pinned like restic.

## Consequences

- Most of the `container` driver's privilege-separation prize is captured — arbitrary
  playbook code can no longer read the database or the keys — with **no podman socket
  handed to `exocompd` and no registry on the DR path**. Landlock is local, so sandboxing
  applies even during a restore.
- The `container` driver's remaining justification narrows to per-project execution
  environment versioning and hard per-job resource caps. It stays deferred.
- **The CVE lesson is the load-bearing part.** nono's CVE-2026-47128 was an escape via the
  user systemd D-Bus socket, fixed in 0.55.0. The escape class generalizes to *"a sandboxed
  process reaches a powerful local unix socket."* Exocomp ships such sockets itself, so job
  profiles grant no socket, and `exocomp doctor` **asserts** the denial rather than assuming
  it — it attempts socket connects from inside a probe sandbox and expects failure.
  container-selinux's socket confinement is treated as a second, independent enforcement of
  the same rule.

  **The assertion found that half of it cannot hold.** Landlock mediates path *opening*,
  and `connect(2)` on a pathname `AF_UNIX` socket opens nothing, so no Landlock ABI shipped
  so far refuses one; abstract sockets, which Landlock scopes, are refused. The denial for
  pathname sockets is therefore enforced by **placement, not policy**: exocomp ships no unix
  socket for a job to name, its own surface is loopback TCP, and no podman socket is mounted
  into the container. `doctor` prints the gap rather than implying a denial it cannot make.
  Recorded in [log.md](../log.md); the requirement to assert instead of assume is what
  surfaced it, and stands.
- `doctor` probes the Landlock ABI (network rules need ABI v4+, kernel ≥ 6.7); under the
  `opportunistic` tier it degrades **loudly** to plain `disabled` with a warning on older
  kernels — never silently — while the default `enforcing` tier fails closed instead.
- Landlock stacks with SELinux (stackable LSM; policies AND together). This is asserted in
  the enforcing CI job, not assumed.
- Fallback seam if the dependency sours: in-process `go-landlock` self-restriction applied
  between fork and exec — no new binary, filesystem-only, weaker network story, same
  profiles.
- nono's agent-oriented features (AGENTS.md signing, snapshot rollback, credential
  injection) are unused.
- Cost: profiles plus doctor probes ≈ 0.5 FTE-week inside the v1.0 hardening line; the
  [bill of materials](../sbom/bill-of-materials.md) gains one pinned ~10 MB binary.
