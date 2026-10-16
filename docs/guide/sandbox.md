---
type: Guide
title: "Job sandboxing"
description: "How exocomp runs jobs under the nono sandbox: tiers, grants, egress, and the privileged opt-out."
status: stable
tags: [guide, sandbox, security]
---

# Job sandboxing

On the supported Linux target, every Ansible, OpenTofu, and `run-cli` job exocomp runs
is executed under a kernel sandbox by default. The sandbox is the reason a playbook — arbitrary code from a git
repository — cannot read the database, the host key, or another job's secrets.

## Tiers

| Tier | When | Isolation |
|---|---|---|
| `enforcing` | default on Linux | kernel default-deny sandbox per job subprocess |
| `disabled` | bootstrap, disaster recovery, `--sandbox disabled` | process boundary, per-job private dir, env allowlist |
| `opportunistic` | `--sandbox opportunistic` | asks for `enforcing`, degrades loudly to `disabled` on an unsupported kernel |
| `container` | deferred | per-job sibling containers |

Select the tier with `--sandbox` or `$EXOCOMP_SANDBOX`. `enforcing` is a policy, not a
preference: when the host cannot deliver it, `serve` and `doctor` refuse rather than run
the fleet's jobs unsandboxed. `opportunistic` is the opt-in that accepts the degradation.

## What a job can and cannot touch

Under `enforcing`, each job gets its own private directory (its `HOME`, `TMPDIR`, and
scratch space) and may read its repo checkout, the tool tree, provider and collection
caches, and the paths its argv names. It cannot read `exocomp.db`, `host.key`,
`keyring.age`, `serve.lock`, other jobs' private directories, or any local socket —
including the exocomp API socket and any podman socket.

`HOME` is redirected into the job's own directory, so an operator's `~/.ansible.cfg` or
`~/.terraformrc` does not reach a queued run.

## Grants

Two opt-in grants widen the profile without weakening it:

- `--ssh-identity <path>` (repeatable) grants read access to **one file** — an SSH
  identity key — never its directory.
- `--source` grants the checkout as the working directory (read, or read+write for a
  writable checkout).

A job that declares nothing keeps the default-deny posture.

## Egress

Network access is scoped by job kind, not by destination:

- Ansible: TCP 22 (SSH to inventory hosts).
- OpenTofu: TCP 443 plus the loopback state-backend port.
- `run-cli`: port 443 only when the registration declared `--egress https`.

Landlock matches ports, not destinations, so per-destination filtering is not available;
a fleet degraded to `disabled` enforces no egress limit at all.

## The privileged opt-out

`--privileged` (on `playbook run`, `run`, `ansible`, `command`, `run-cli`, or the web
toggle) runs one job unsandboxed — the `disabled` tier, full host access. It is
operator-gated, recorded on the job row, and surfaced after the fact: the job detail
wears an `unsandboxed` chip, the list names it beside the source, the log opens with an
attributed band, and `jobs list --output json` carries `privileged`. Reaching a host over
SSH is the sandbox-preserving default; reserve `--privileged` for work that must run
unsandboxed on the controller itself.

## Operational limits

- The data directory's `run` subdirectory must stay under 57 bytes: sandboxed jobs
  coordinate over unix sockets placed inside it, and `AF_UNIX` addresses cap at 108.
  `exocomp doctor` measures it at startup.
- `exocomp doctor` reports the nono launcher version against its floor, the kernel's
  Landlock ABI (network rules need ABI v4+, kernel 6.7+), the tier in effect, and the
  result of a live canary that asserts the denials against real targets.

## See also

- [Ansible automation](ansible.md) — `--privileged`, `--ssh-identity`, `--check`.
- [Execution driver](../architecture/execution.md) — full tier and grant mechanics.
- [Decision 0005](../decisions/0005-job-sandboxing-nono.md) — why nono.
- [Security threat model](../security/threat-model.md) — sandbox as a control.
