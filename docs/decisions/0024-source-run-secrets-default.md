---
type: Decision
title: "Decision 0024 — Source-scoped default run-secret binding"
description: "Persist an operator-set run-secret binding name on a source and fill it into every ansible run that names the source and passes none, at queue admission; repo content can never choose a binding."
status: stable
tags: [decision, secrets, credentials, queue, sources]
---

# Decision 0024 — Source-scoped default run-secret binding

Status: accepted · 2026-10-24

## Context

A run-secret binding exports sealed values as named env for one run
([Decision 0010](0010-source-and-credential-model.md)). It is a per-invocation
choice: `playbook run`, `ansible` and `command` accept `--run-secrets`, and a
playbook rule cannot carry one ("a plain, vault-less, inventory-discovery play
for v1"). A git source has no untracked vault password file, so a tree with
encrypted vars decrypts only when the password reaches ansible as an env var
through a binding. A rule-fired play against such a source therefore failed when
the rule fired, not when it was registered, and an operator had no place to record
"every run of this source needs this credential".

[Decision 0023](0023-source-egress-defaults.md) settled the same question for
egress: an operator-set default on the `source` row, applied at queue admission.
This record applies that shape to the run-secret binding name.

## Decision

**Persist an operator-set default binding name per source, and fill it into every
ansible run that names that source and passes no binding of its own.**

- The name lives on the `source` row (`source.default_run_secrets`, migration
  `0044_source_run_secrets.sql`), written only through an operator-scope face:
  `exocomp source run-secrets <source> [<binding>]` and the source detail page.
  An empty value clears it. A non-empty value must name a binding that holds at
  least one pair, so a typo fails at registration.
- Only the name is stored. The pairs stay in `run_secret`, the values in the
  envelope, and both are resolved when the run starts, exactly as for an explicit
  binding. Nothing secret is added to the new column.
- The fill happens in `queue.ApplySourceDefaults`, which `Enqueue` and
  `EnqueueGated` call next to the egress union, gated on `Kind == KindAnsible &&
  Source != ""`. The inline drain of a daemonless CLI run calls it before it
  resolves the binding, because that path reads the spec before the queue admits
  it. Every producer therefore inherits the default: a manual `playbook run`,
  `ansible`, `command`, a rule-fired playbook, an API run.
- An explicit `--run-secrets` always wins. The default is never merged with it: a
  run that names a binding gets that binding alone.
- The job row records the effective binding in `run_secret_binding`, the field an
  explicit binding already uses, so the job detail and every audit read show what
  the run received. A rerun or redrive clones that recorded binding.
- A source's content can never choose a binding. No checkout file,
  `.exocomp/hooks.yml`, inventory, extra-var, or event payload contributes one.
- `--vault-project` is not defaulted. It derives a password from the envelope for
  a project name, a different mechanism with its own per-run argument; the two
  compose when both are set. A source whose encrypted tree reads its password from
  an env var uses this default; a source that uses `--vault-project` keeps
  passing it per run.

The sandbox is unchanged. The binding is injected into the same allowlisted
environment an explicit one is, the redactor scrubs the same plaintexts, and the
resolver still refuses a value shaped like an access token.

## Consequences

- A rule-fired or API-fired play against a source with encrypted vars can run
  without a human typing a flag, and the failure moves to registration when the
  binding is unknown.
- **Unattended runs now hold credentials.** Rules were credential-free by design.
  A source with a default hands that binding to every run that names it, including
  rules fired by webhooks, checks and host-token requests. The grant is as wide as
  the binding and as trusted as the source's checkout, which a signed, pinned
  source constrains and an unpinned one does not. The rules page therefore renders
  a `secrets <binding>` guard for each playbook rule whose source has a default,
  the way it renders effective egress. Operators choose per source; a source with
  no default behaves exactly as before.
- Operator scope only. A readonly or approve token, an agent tool, and a host
  token cannot set or clear the default. There is no API verb for it, as
  for the egress default.
- `run-secret rm` refuses a binding that a source names as its default, so the
  default cannot be orphaned by a removal. There is no per-run opt-out: a run can
  replace the default with its own `--run-secrets` but cannot skip it. A binding
  whose secrets are later deleted still fails the run with the "not found" error
  an explicit `--run-secrets` gives, rather than running without credentials.
- A scheduled play carries no source (`ScheduleArgs` has no `Source`), so it does
  not inherit the default, the same limit Decision 0023 records for egress. Giving
  a schedule a source, with the checkout, `ansible.cfg` and hook handling a
  source-resolved run needs, is a separate change. Until then a play against a
  source tree with encrypted vars belongs in a playbook rule.
- A rerun, redrive or workflow re-arm clones the job's stamped binding and then
  passes admission again. Clearing the default does not strip a binding already
  stamped on an older job, and a job stamped with none picks up the source's
  current default.
- A handoff or bootstrap play carries no source on its spec, so it does not
  inherit the default either.
