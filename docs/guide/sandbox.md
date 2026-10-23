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
scratch space) and may read its repo checkout, the tool tree, the managed-Python install
dir (`UV_PYTHON_INSTALL_DIR`, when set and outside the data dir), provider and collection
caches, and the paths its argv names. It cannot read `exocomp.db`, `host.key`,
`keyring.age`, `serve.lock`, other jobs' private directories, or any local socket —
including the exocomp API socket and any podman socket. A socket an operator mounts into
the container is not refused, and a job can reach it.

`HOME` is redirected into the job's own directory, so an operator's `~/.ansible.cfg` or
`~/.terraformrc` does not reach a queued run.

## Grants

Two opt-in grants widen the profile without weakening it:

- `--ssh-identity <path>` (repeatable) grants read access to **one file** — an SSH
  identity key — never its directory. A `--remote` run may name only files the daemon's
  operator allowed with `serve --allow-ssh-identity`.
- `--source` grants the checkout as the working directory (read, or read+write for a
  writable checkout).

A job that declares nothing keeps the default-deny posture.

## Egress

A job's network access is scoped by **port, never by destination**. The dependency
resolvers are the one exception, below. A run's effective reach is
additive — the kind's built-in port, plus any operator-set default on the source it names,
plus whatever the run (or the rule that triggered it) declares:

- **Ansible:** TCP 22 (SSH to inventory hosts), widened by a run's `--egress` ports, by a
  playbook rule's declared ports, and by the named source's operator-set default
  (`exocomp source egress <name> [port...]`), which every ansible run naming that source
  inherits at queue admission ([Decision 0023](../decisions/0023-source-egress-defaults.md)).
  The de-duplicated union is what the job row records and what the approval page and the
  rules page render.
- **OpenTofu:** TCP 443 plus the loopback state-backend port, widened by a workspace's
  operator-set default (`exocomp workspace egress <name> <port>...`).
- **`run-cli`:** port 443 only when the registration declared `--egress https`.
- **Dependency resolvers (`uv`, `ansible-galaxy`):** named hosts only, through nono's
  proxy and not a port grant. `uv` gets `pypi.org` and `files.pythonhosted.org`,
  `ansible-galaxy` gets `galaxy.ansible.com` and its download bucket, and
  `serve --resolver-allow-domain <host>` (repeatable) adds more, for a private or alternate
  index or a host named in a collections requirements file (a `type: git` or URL entry, or
  a non-default `server`). The flag configures the daemon, so a daemonless `playbook run`
  uses the defaults. `uv` does its own name lookup, which the sandbox blocks, and the
  proxy resolves names for it. A wildcard is refused: the proxy matches names, so a name
  that resolves to loopback would reach the daemon's own listeners. Raw TCP stays blocked.
  `exocomp doctor` asserts that under `proxy mode`, and where it fails the resolvers
  refuse to run while every other job stays sandboxed.

Landlock matches ports, not destinations, so a granted port is reachable to any host the
run can reach. Only the resolvers' proxy filters by host. A fleet degraded to `disabled`
enforces no egress limit at all.

## The privileged opt-out

`--privileged` (on `playbook run`, `run`, `ansible`, `command`, `run-cli`, or the web
toggle) runs one job unsandboxed — the `disabled` tier, full host access. It is
operator-gated, recorded on the job row, and surfaced after the fact: the job detail
wears an `unsandboxed` chip, the list names it beside the source, the log opens with an
attributed band, and `jobs list --output json` carries `privileged`. Reaching a host over
SSH is the sandbox-preserving default; reserve `--privileged` for work that must run
unsandboxed on the controller itself. An unsandboxed ansible job keeps the daemon's HOME
but still gets its local temp and ssh control directory under a per-job directory and its
remote temp under `$HOME/.ansible/tmp`, so a read-only source checkout does not fail the run.

It sets the tier of the job alone. The dependency resolvers, the per-source `uv` venv and
the `ansible-galaxy` collections, follow the sandbox tier of the process that runs the
job, the daemon's for a queued or remote run, and stay confined under `enforcing` even
when the job is privileged. `--privileged` with `--remote` is refused, because a remote
run takes the daemon's tier.

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
