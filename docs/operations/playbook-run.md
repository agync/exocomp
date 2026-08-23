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
executing a tree the signature never covered (finding AI-12,
[threat model](../security/threat-model.md)). Run a playbook from elsewhere without
`--source`.

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

Related: [Ad-hoc runs and the unreachable-tolerant outcome](adhoc-and-partial.md) ·
[Reacting to alerts](reactive.md) · [Gating tofu plans](plan-policy.md) ·
[Provision → onboard → converge](provision-converge.md) ·
[Run lifecycle: hooks and the gate](run-lifecycle.md).
