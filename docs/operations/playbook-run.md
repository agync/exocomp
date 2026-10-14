---
type: Guide
title: "Playbook run flags and config-drift"
description: "The playbook run flags, where extra-var values live, and config drift as a check or schedule outcome."
status: stable
tags: [operations, ansible, playbook]
---

# Playbook run flags and config-drift

`exocomp playbook run <playbook>` enqueues one ansible job and streams its
events; whichever process runs it — the CLI draining its own queue, or a running
`exocomp serve` — derives the vault password the same way. These flags shape what
ansible does, and one of them turns a run into a config-drift probe.

## The passthrough flags

| Flag | Reaches ansible as | Notes |
|---|---|---|
| `--limit <pattern>` | `--limit <pattern>` | restrict the play to a host pattern |
| `--tags <list>` | `--tags <list>` | only tasks carrying these tags |
| `--skip-tags <list>` | `--skip-tags <list>` | skip tasks carrying these tags |
| `--check` | `--check` | dry run; emits the drift outcome (below) |
| `--diff` | `--diff` | show what would change; pairs with `--check` |
| `-e/--extra-var` | see below | repeatable; `key=val` or `@file` |

They are recorded on the job row, so a daemon that claims the row runs exactly
what the CLI enqueued. They keep working with `--source` (a source with no hooks
runs from its checkout; a hooked source composes the same flags into its
lifecycle workflow's play step) and with a registered or `tf:<workspace>`
inventory.

With `--source`, the playbook path is **confined to that source's checkout**: it must be
relative, and a path that resolves outside the tree (`../`, or through a symlink) is refused
before the run is enqueued. That is what keeps a run recorded against a signed source from
executing a tree the signature never covered (see the
[threat model](../security/threat-model.md)). Run a playbook from elsewhere without
`--source`.

A `--source` run also executes **with the checkout as its working directory**, so the
tree's own `./ansible.cfg` — and the relative `inventory`, `roles_path`, and
`collections_path` it names — resolve exactly as a bare `ansible-playbook` from that
directory would. This is the same working-directory grant the lifecycle
[`exocomp run`](run-lifecycle.md) path gives its play step, and it carries the source's
read-only posture: a read-only checkout is granted read, never write, so the run reads its
own tree but cannot write back to it. The sandbox redirects ansible's scratch (local temp,
remote temp, and the ssh control-path directory) into the per-job private tree, so a repo
`ansible.cfg` with a relative `local_tmp` or `control_path_dir` still writes somewhere
granted rather than onto the read-only checkout. A run with no `--source` keeps today's
behavior: it executes in the executor's per-job scratch directory, and ansible discovers
its own configuration from there.

`--privileged` runs the job **unsandboxed** — the `disabled` tier, full host access — even when
the daemon default is `enforcing`. It is operator-only, off by default, and audited; use it only
for trusted, operator-authored work that genuinely needs host access. See the per-run override in
[serve.md](serve.md) and [security/threat-model.md](../security/threat-model.md).

`--ssh-identity <path>` (repeatable) grants the run **read access to an ssh identity key
file stored outside the source checkout** — a fleet key the operator does not want copied
into the tree. Each path is granted as a single file, never a directory, and the declaration
is per run and empty by default; a run that declares nothing keeps the sandbox's default-deny
posture. The key file must be named by an absolute (or working-directory-relative) path, and
`exocomp run --ssh-identity` grants the same files to a lifecycle workflow's play step.
See [serve.md](serve.md) for the security posture.

## Extra vars, and where their values live

`-e/--extra-var` is repeatable and takes two forms:

- **`-e key=val`** — the value is carried on the job row and written to a file in
  the run's private directory, handed to ansible as `--extra-vars @<file>`. It is
  **never placed on ansible's own argv**, so a value does not surface in the
  process table. The variable name is validated (`[A-Za-z_][A-Za-z0-9_]*`) so a
  crafted key cannot split into extra flags; `key=val` takes precedence over a
  `@file` variable of the same name.
- **`-e @file`** — passed through as `-e @<abspath>`. Only the path travels in
  argv; the values stay in the file. The file must exist, or the run is refused
  before it starts.

What never travels in argv: the ansible-vault password (a private-dir file) and
run-secret / dynamic-inventory credentials (named env resolved from the
envelope). Those out-of-argv paths are unchanged by these flags.

**Trust posture.** A `-e key=val` value is operator-supplied run configuration.
It is written to the plain job row and to a run-private file, at the same trust
level as the operator's own shell history — it is not an envelope-sealed secret.
For a genuinely sensitive value (a token, a DB password) use a sealed
[run-secret](sources.md) or an ansible-vault variable via `--vault-project`,
not `-e`.

## `--check`: config drift as an outcome

`--check` runs ansible in check mode: no host is changed. After the run, exocomp
parses the `PLAY RECAP` and attaches a structured **config-drift outcome** to a
job event — the check-mode twin of a tofu plan's drift outcome:

```
ansible check: 2 host(s) clean, host web1: 3 task(s) would change
```

The event's `raw_json` carries the machine form under `ansible_check`:

```json
{"ansible_check": {"operation": "check", "changes": true,
  "clean_hosts": 2, "drift": [{"host": "web1", "changed": 3}]}}
```

Only the **count** of would-change tasks travels per host, never a task's diff —
a diff can hold a secret, so the resource-level signal is "how much drift", never
"what to". A normal (non-check) run is unaffected: it emits no `ansible_check`
event and behaves exactly as before. A run that errors before the recap prints
emits no outcome rather than a false "everything clean".

## The config-drift schedule

To detect config drift regularly, register a **check-mode play schedule** — the
config twin of a tofu-plan drift schedule:

```
exocomp schedule add nightly-drift --kind play --check \
  --target tf:edge --cron "0 0 3 * * *" -- site.yml
```

`--check` on a `--kind play` schedule appends `--check --diff` after the
playbook, so every fire runs in check mode against the standing inventory
`--target` names (a `tf:<workspace>` inventory a workspace's apply refreshes, so
a newly provisioned host joins the next drift sweep) and emits the same
`ansible_check` outcome. The play touches no host state; it only reports what has
drifted. The fail-closed rule for play schedules still holds: a missing, stale,
or hostless inventory fires nothing rather than reporting a clean sweep against
nobody. `--check` is rejected on any other kind.

## The reactive seam

Both drift planes flow through one control plane. Infra drift (`tofu plan`
`-detailed-exitcode`) becomes a `TofuDrift` reactive event via the engine's drift
sweep; config drift is emitted as the `ansible_check` job event above. The
producer-side helper `executor.AnsibleCheckChanges(rawJSON)` reads the outcome
back out in the same shape `executor.TofuPlanChanges` gives the tofu sweep, so an
`AnsibleDrift` sweep can mirror `SweepDrift`: scan the succeeded ansible jobs,
read each one's events through `AnsibleCheckChanges`, and raise an event when
`Changes` is true — the same pipeline a webhook enters, where a rule can notify
or gate on it. Wiring that sweep is the remaining step; the outcome and the
read-back helper are in place for it.

## Sandbox denial hints: self-host `become`, keys, and `--privileged`

Two failures of a sandboxed ansible run look like broken systems but are the sandbox
enforcing its policy. Both append one operator-facing hint to the failed run's events; the
exit code is unchanged (invariant 3), and the hint is diagnosis only — the denied operation
stays denied.

### Privilege escalation on a self-host target

When a playbook targets the host the daemon runs on, the inventory may resolve the local host
to `connection: local` (the homelab inventory does this dynamically when the inventory
hostname is the local one). With `become` enabled, the escalation's `sudo` then runs
**inside** the sandbox, not over SSH. The sandbox is default-deny and grants no setuid sudo's
private libraries, so the loader reports a missing `libsudo_util`, and the failed run gains:

```text
the sandbox denied privilege escalation (sudo) — become's sudo ran inside the
sandbox; target the host over SSH instead of a local connection so the
escalation runs on the far side, and only as a last resort re-run with
--privileged (operator-gated, unsandboxed)
```

When the run's inventory is the controller itself — `--inventory localhost,`, or an ad-hoc
loopback pattern with no inventory — the hint names the connection type as the cause:

```text
the sandbox denied privilege escalation (sudo) — this run reaches the host over
a local connection, so become's sudo executed inside the sandbox; point the
inventory at the host over SSH (ssh-to-self) so the escalation runs on the far
side, and only as a last resort re-run with --privileged (operator-gated,
unsandboxed)
```

The sandbox-preserving fix is to reach the host over SSH. A remote target runs its own `sudo`
on the far side, outside the sandbox; for the controller's own host that means **ssh-to-self**
— point the inventory at the host over its SSH address so ansible connects to it as a remote
rather than over a local connection. `--privileged` runs the job unsandboxed (the `disabled`
tier, full host access); it is operator-only, off by default, and audited, and the hint names
it last.

### An ssh identity key outside the checkout

The default profile allows TCP 22 egress but grants no read of `~/.ssh`, so a run that must
load a private key fails at the ssh layer (`Load key "…": Permission denied`, `Permission
denied (publickey)`, or `no such identity`). The failed run gains:

```text
the sandbox denied reading an ssh identity key — grant the key's path with
--ssh-identity <path> (a single-file read grant, never its directory) and re-run
```

The sandbox-preserving fix is `--ssh-identity <path>`, which grants read access to **one**
identity file and never its directory. It is available on `playbook run`, `run`, `rule add
--action playbook`, `command`, and `ansible`, so a one-off against a key-only host does not
have to reach for `--privileged`. A run that already declared `--ssh-identity` is not offered
the hint again, because the key it named is not the one that failed.

The sandbox is never widened to "allow sudo" or to grant `~/.ssh`: a sandbox that permits
escalation, or a whole directory of keys, is not a sandbox. Each is an explicit, per-run,
operator decision.

When ssh-to-self is not possible and the run genuinely needs host access, two ways remain:

1. **Run the self-host part separately, as privileged.** Keep the ordinary
   playbook run covering the remote hosts (for example `--limit` that
   excludes the self host), then queue a separate run for the self-host /
   elevated part with `--privileged`.
2. **Run the whole thing privileged.** Mark the single run elevated:
   `--privileged` on the CLI, or the operator-gated `privileged` toggle on the
   web rerun / source launcher. This is an explicit, operator-scoped opt-out
   of the kernel sandbox for one run; it cannot enforce per-job caps, so a
   run that also names a runner is refused rather than run uncapped.

Both are visible after the fact: the job detail wears an `unsandboxed` chip,
the jobs list names it beside the source, the rerun dialog states it and starts
the escalation off, the TUI ledger marks the row, `jobs get` and
`jobs list --output json` carry `privileged`, the log opens with the attributed
enqueue band, and `exocomp doctor` reports the privilege posture as a finding.
Reaching the host over SSH (option 1's remote half) is the sandbox-preserving
default; `--privileged` is for the run that must be unsandboxed on the
controller itself.

Related: [Ad-hoc runs and the unreachable-tolerant outcome](adhoc-and-partial.md) ·
[Reacting to alerts](reactive.md) · [Gating tofu plans](plan-policy.md) ·
[Provision → onboard → converge](provision-converge.md) ·
[Run lifecycle: hooks and the gate](run-lifecycle.md).
