---
type: Reference
title: Platform integration contract
description: "The support tiers for systemd and Quadlet, rootless podman, docker-compose and SELinux enforcing, plus the specified remote-access path."
status: stable
tags: [architecture, platform, containers]
---

# Platform integration contract

"Works with" is a tested contract, not an adjective. Each platform below has a tier; the
CI enforcement named per row is **designed but not implemented** — the repository runs no
CI, so these tiers are commitments the delivery plan states, not verified claims.

| Platform | Tier | What makes it true |
|---|---|---|
| systemd (Quadlet) | first-class | `Type=notify` + `WatchdogSec`. Readiness is signalled by `sd_notify` **after** DB integrity check and keyring unwrap; the watchdog is pinged from the scheduler loop, so a *wedged* daemon is restarted, not merely a dead one. The installer runs `loginctl enable-linger`; `MemoryMax=` lives in the unit; logs go to journald |
| podman (rootless) | first-class | Quadlet units, podman ≥ 4.4 floor. Podman auto-update is **disabled** for our unit — self-update is the approval-gated job instead. Socket mounting is confined to the deferred `container` driver |
| docker / compose | supported, degraded loudly | shipped compose file; `restart: unless-stopped` and json-file logs instead of systemd. **No `systemd-creds` path, so the host key is a 0400 file and TPM sealing is unavailable.** Rootful unless the operator runs rootless Docker. The shipped compose file states the degraded tier rather than pretending parity |
| operator-built image | supported, base-only | inherit `FROM ghcr.io/abn/exocomp` with version-pinned layers; the built image is captured into the recovery set (`podman save`) or recorded by Dockerfile + source base digests. Copying our artifacts into a foreign base with `COPY --from` is deferred ([Decision 0016](../decisions/0016-artifact-carrier-image.md)) |
| SELinux enforcing | supported, CI-tested | see below |

The degraded Docker tier is the design's honesty test: it is documented as a lower threat
tier ([Decision 0003](../decisions/0003-secrets-envelope.md) — the host key protects against off-host
attackers and stolen disks, not root on the box) rather than quietly shipped as equivalent.

## SELinux

The target platform includes the Fedora/RHEL family, enforcing. No example anywhere in this
bundle may depend on permissive mode.

- **Labels everywhere.** The data volume is declared via a Quadlet `.volume` unit (implicit
  relabel) and every documented bind mount carries `:Z`.
- **The NAS leg is the sharp edge.** Network filesystems cannot be relabelled — `:Z` fails
  on NFS and CIFS. The installer sets and verifies the `virt_use_nfs` / `virt_use_samba`
  booleans, and the mount canary turns an AVC-denial `EACCES` into a loud pre-backup failure
  rather than a silently missing backup leg.
- **nono/Landlock stacks with SELinux.** Both are LSMs and their policies AND together;
  this is asserted in the enforcing CI job, not assumed
  ([Decision 0005](../decisions/0005-job-sandboxing-nono.md)).
- **container-selinux's socket confinement** is treated as a second, independent
  enforcement of the "jobs reach no local sockets" rule. The one place it bites is
  documented: the deferred `container` driver's podman-socket mount needs `label=disable`
  on that mount.
- **CI gate.** A Fedora VM job runs install → bootstrap → backup → restore drill with
  `setenforce 1`. SELinux support that is not tested enforcing is a wish, by the same rule
  that applies to the RSS budget. `exocomp doctor` surfaces recent AVC denials touching our
  paths.

## Remote access

Local access is a unix socket, which asks no auth question. The approval flow needs an
endpoint reachable from a phone, so the transport is specified rather than assumed:

- **Tailscale/WireGuard first.** The wizard detects a tailnet and configures the TCP
  listener bound to the tailnet address, with tailnet URLs in notifications and optional
  Tailscale Serve for TLS.
- **Fallback**: a Caddy + ACME reverse-proxy recipe shipped in the docs.
- Approval callbacks are HMAC single-use, TTL-bounded and constant-time compared — but they
  are bearer capabilities, so ntfy defaults to self-hosted or token-authenticated and the
  wizard never defaults to a public topic.
