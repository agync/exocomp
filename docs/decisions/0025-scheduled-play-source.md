---
type: Decision
title: "Decision 0025 — A play schedule can name a source"
description: "A kind-play schedule may carry a registered source: the play runs from that source's checkout and inherits its default egress and run-secret binding at queue admission; a schedule without one is unchanged."
status: stable
tags: [decision, schedules, sources, queue, secrets]
---

# Decision 0025 — A play schedule can name a source

Status: accepted · 2026-10-24

## Context

[Decision 0023](0023-source-egress-defaults.md) and
[Decision 0024](0024-source-run-secrets-default.md) fold a source's operator-set
defaults into every ansible run that names the source, in `queue.ApplySourceDefaults`
at queue admission. A scheduled play named no source: `ScheduleArgs` had no field for
one, the playbook was a path used as given, and the run had no working directory, so
the checkout's `ansible.cfg` never loaded. Both records said so and left it as a
separate change. The consequence was that a recurring converge or drift check against a
source tree with encrypted vars, or one that needs a port beyond SSH, had to be
rebuilt as a playbook rule.

## Decision

**A kind-play schedule may name a registered source, and a fire resolves the source
the way a playbook rule does.**

- `schedule add --kind play --source <name> --target <inventory> -- <playbook>`. The
  source must resolve when the schedule is added, as `rule add` requires of a
  playbook rule: it must exist, carry no hooks, and the playbook must be relative to
  its checkout and stay inside it. `--source` on any other kind is refused.
- The name is stored in `args_json` beside the schedule's other per-kind fields
  (`target`, `tolerate_unreachable`), as `source`, omitted when empty. No column and no
  migration: a row written before the field decodes to no source, and a schedule
  added without `--source` stores the same bytes it did before.
- At fire time the scheduler resolves the source through
  `service.ResolvePlainSourcePlay`, the resolution `BuildPlaybookSpec` uses for a
  rule-fired playbook. The playbook is confined to the checkout, the checkout root is
  the run's working directory (so `ansible.cfg` loads) with the source's read-only
  posture, and the spec names the source. `ApplySourceDefaults` then unions the
  source's default egress and fills its default run-secret binding, as for any run
  that names it.
- The inventory is the standing one, not the checkout's own. `--target` still names a registered
  inventory, the argv still carries no `--inventory`, and the fail-closed rules
  (missing, stale or hostless inventory fires nothing) still hold.
- A source with hooks or a gate is refused, as for a rule: an unattended play never
  composes the lifecycle workflow `exocomp run` owns. A source that cannot be
  resolved at fire time, including one deleted since the schedule was added, enqueues
  nothing and logs why; it does not fall back to a run without the defaults.
- Run now (web and TUI) builds the spec through the same `sched.Specs`, so a manual
  run of a schedule carries the same source, directory and defaults as a fire.

A schedule with no source behaves exactly as before: the playbook path as given, no
working directory, no source, no defaults.

## Consequences

- A scheduled play against a source can decrypt an env-var-keyed vault tree and reach
  the source's extra ports without being rewritten as a rule. The credential
  consequences of Decision 0024 apply: an unattended, cron-driven run now holds the
  source's default binding, as wide as that binding and as trusted as the source's
  checkout.
- No referential check ties a schedule to its source. `source rm` does not look at
  schedules, as it does not look at rules; the next fire fails closed and the log names
  the missing source.
- Because the inventory is the registered standing one, the driver passes it as
  `--inventory`, which overrides the checkout's `ansible.cfg`. Vars beside the
  playbook and in the tree's roles load; `group_vars` and `host_vars` that sit beside
  the checkout's own inventory do not. A tree whose vault-encrypted vars live there is
  not served by a scheduled play and stays a playbook rule. A schedule also has no
  `--vault-project`: the credential is the source's default binding.
- Run-secret layering is unchanged: tofu auto-outputs, then the binding, then the
  inventory's env binding, later layers winning
  ([sealed tofu outputs](../operations/sealed-tofu-outputs.md)). Adding `--source` to a
  `tf:<workspace>` schedule lets the source's binding shadow a sealed apply output of
  the same env name.
- A fire that cannot resolve its source is visible only in the daemon log, and last_run
  stays unstamped so catch-up retries it. Repo content can trigger it: a commit that
  adds a hooks file makes the source's scheduled plays refuse. A missing inventory
  already behaves this way.
- The web add-schedule form has no source field; a sourced schedule is set from the
  CLI, and the form ignores a stray `source` value.
- A handoff or bootstrap play is still sourceless. Those are workflow chains built by
  the CLI: their `--source` supplies lifecycle hooks around the chain, and the play
  step carries no source or working directory of its own (a provision chain has no
  `--source` at all). Letting the step inherit the source's defaults would change what
  an existing chain is granted and needs the same hook and checkout handling as a
  source-resolved run, so it is a separate decision, not part of this one.
- The schedules page and the schedule detail name the source and link to it; the
  `schedule.list` JSON carries it as `source`.
