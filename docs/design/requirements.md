---
type: Design
title: "Requirements"
description: "The two hard constraints, the six goals, the anti-scope contract, and what is deferred with a named seam."
status: stable
tags: [design, scope]
---

# Requirements

Scale is fixed first, because it decides everything else: **one operator, a handful of
hosts.** Not a team, not a fleet, not a tenant model.

## The two hard constraints

Every requirement below must survive both. A feature that cannot is cut, not deferred.

1. **Trivial bootstrap.** Zero to a working, proven install in minutes, from one command,
   with no manual key ceremony and no hand-edited configuration files.
2. **Trivial disaster recovery.** A dead host plus a printed kit plus any one backup target
   is a full recovery — with no vendor, no internet, and no exocomp code required on the
   path.

The second constraint is the one that rules. It is why nothing is stored in a bespoke
format, why the installer is versioned separately from the daemon, why the printed recovery
card is CI-executed rather than written, and why the DR path is pinned to the image rather
than to whatever is in the data volume.

## The six goals

| # | Goal | What it means | How the design answers it |
|---|---|---|---|
| 1 | **Run engine** | Playbook runs and plan/apply runs, inventory, schedules, job history — the AWX and Terraform Cloud roles in one place | One SQLite job queue with live event streams; `ansible-playbook` (v0.1) and `opentofu` as pinned subprocesses ([overview](../architecture/overview.md)) |
| 2 | **Secrets** | A real root of trust feeding ansible-vault, sops files, and encrypted OpenTofu state — recoverable from one printed key | age recipients → DEK → HKDF subkeys, so the printed kit carries one secret ([ADR 0003](../adr/0003-secrets-envelope.md)) |
| 3 | **Backup** | Encrypted, verified backup of all control-plane state to a NAS, S3-compatible storage, and a private GitHub repo | Three independent restic/git legs from one staging tree, rotating verification, and a monthly automated restore drill ([disaster recovery](../operations/disaster-recovery.md)) |
| 4 | **Reactive runbooks** | Alerts from Alertmanager or Uptime Kuma trigger runbooks — behind approval gates, cooldowns, and caps | Normalized webhooks → CEL rules → guardrails enforced *in the queue* → ntfy-tappable approvals → the same job path as manual runs |
| 5 | **TUI first** | Terminal UI as the primary interface, web second, desktop third | All three are thin clients of one API; business logic exists only in `internal/service`. Bubble Tea v2, also served over SSH via Wish |
| 6 | **Featherweight** | A single container on a 4 GB N100 or Raspberry Pi 4 | Budgets enforced in CI, not asserted in a README ([bill of materials](../sbom/bill-of-materials.md)) |

## Anti-scope

The failure mode this project is most likely to die of is becoming a worse AWX. AWX went
there and now needs 16 GB of RAM and has not shipped since July 2024. The constraint is
therefore written down as a contract:

**One operator, a handful of hosts, one container, no external database — ever.**

Concrete rejections, stated up front:

- No RBAC beyond `operator` / `readonly`.
- No multi-tenancy, no surveys.
- No workflow DAGs. A runbook that needs a DAG is a playbook with `import_playbook`.
- No plugin system. A notification target `shoutrrr` lacks is a generic webhook.
- No multi-node HA.

The general rule: **if a feature needs a second process or a second database, it is out of
scope by definition.** The idle-RSS budget is enforced by CI for the same reason — a number
in a test is a constraint; a number in a README is a wish.

## What is deliberately deferred

Wails desktop, an OpenBao secret resolver, Litestream embedding, and GoAlert-class
escalation. Each has a named seam rather than a promise: `secret.kind = external` holds a
reference instead of a value, the envelope's key-provider is an interface an external KMS
or OpenBao Transit key can implement, and a `notifier` interface is reserved for
escalation. On-call rotations are explicitly out of scope, not deferred.
