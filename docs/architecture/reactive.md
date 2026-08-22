# The reactive layer

A webhook is the fourth headless client of the one queue. Nothing in this layer
executes anything: it decides what an arriving event is allowed to enqueue, and
records that decision whatever it turns out to be.

```
POST /hook/<token>  ──▶  normalize  ──▶  CEL rules  ──▶  guardrails  ──▶  queue
                                                              │
                                                              └──▶  approval ──▶ queue
```

Every path ends in exactly one `event` row carrying an outcome, a reason, and
the per-condition trace that produced them. An event that vanished because a
guardrail swallowed it is the failure mode the table exists to prevent.

## Ingress

`POST /hook/{token}` is mounted on the same loopback listener as the OpenTofu
state backend, so there is one address and one authentication story.

The path segment is the source's token: `Derive("webhook-hmac", <source>)`
rendered as 20 hex characters. It is derived rather than stored, so a restore
rebuilds it, a rotation invalidates it, and `exocomp serve` can print it at
startup instead of the operator keeping it somewhere. Three sources are
normalized: `alertmanager`, `healthchecks`, and `generic` JSON.

A sender that can sign presents `X-Exocomp-Signature: sha256=<hex>` over the raw
body under the same subkey; a header that is present and wrong is a refusal, not
a warning. Alertmanager and healthchecks cannot sign, so a signature is not
required — refusing them would mean refusing the two sources this exists for.
`--hook-allow` narrows senders to a CIDR list on top of that.

## One event shape

Each source collapses onto `source, kind, name, status, severity, host,
instance, fingerprint, summary, labels, annotations`. `status` is `firing` or
`resolved` and nothing else: a monitor saying something this code has not seen
before is not evidence that the problem went away, so anything unrecognized is
`firing`.

The dedup key identifies "the same condition" and deliberately excludes the
status, because a resolved alert has to find the approval its firing raised. It
is the source's fingerprint when there is one, else `source:name:host`.

## Rules

A rule is a CEL expression over `event`, a source filter, an action, and the
guardrails that bound it. The environment holds exactly one variable and the
standard operators — nothing that touches the host — and it is built once, so
what `rule add` accepts is exactly what the engine will run. An expression is
compiled at `rule add` (a rule that would never run is refused at write time)
and again when the engine loads rules; a row that stopped compiling is reported
and left out rather than silently skipped.

Evaluation is bounded by a hard time budget and a cost limit: CEL is not
Turing-complete, but its macros are still loops over whatever a sender put in a
label set. An expression that errors does not match — a rule that cannot decide
must not be able to fire an apply.

Actions: `shell` (argv), `tofu-plan` and `tofu-apply` (a workspace), `notify`
(no job at all).

## Guardrails, in ledger order

They run in this order and the order is the contract: a silenced host must not
spend a rule's daily cap, and a duplicate must not reset a cooldown.

| Step | What it asks | Reason string it writes |
|---|---|---|
| silence | does an active silence cover this event? | `silenced: host web-01 until 14:32 (12 swallowed)` |
| dedup | has this condition already been decided inside the window? | `duplicate of event 41 within 5m` |
| cooldown | did this rule fire too recently? | `cooldown 6h, next eligible 14:32` |
| daily cap | has this rule spent its budget? | `daily cap 5 reached; rule restart disabled` |
| approval | is the action destructive, or does the rule require one? | `approval 3 pending, expires 14:32` |

Silences are rows with a mandatory `expires_at`; there is no never-expires form,
and expiry is decided at read time so nothing has to sweep for one to lift. The
global scope is the reactive kill switch — one row, not a separate flag, so
pausing everything obeys the same mandatory expiry as muting one alert.

Cooldowns and caps read `rule_fire` rows rather than counters in memory, so a
crash-loop restart does not hand a rule a fresh budget. A cap **disables** the
rule and pages at max priority: a cap that throttled silently would leave a
misfiring rule firing forever at the cap rate.

A dedup window anchors on the last event with the key that was *not* itself
swallowed. Anchoring on the suppressed repeat would slide the window forward
forever and never let the alert through again.

## Approvals

`tofu-apply` is destructive and is gated whatever the rule says; any rule can
opt in with `require_approval`. A gated action's job is inserted straight into
`awaiting_approval` — never `queued` first — because a job that exists as
queued for even an instant is claimable.

The decision link is an HMAC of the job id under `Derive("approval", "")`,
base64url-encoded. It is unforgeable without the envelope and cannot be replayed
against another job. It is derived rather than random so the 50%-TTL re-publish
and `exocomp approval link` rebuild the same link without the daemon having kept
it; single use is the approval row's state, not the token's secrecy over time.
Both the token and its stored hash are compared in constant time, and the row
stores only the hash — a copy of the database approves nothing.

Against the [approval surface contract](../design/ui-brief.md):

1. **Terse lock screen** — the notification is a count and a TTL. It names no
   host, workspace, playbook, or resource.
2. **Fail-closed countdown** — the TTL rejects the job. It is checked on the
   decision path as well as by the sweep, so an unswept daemon still refuses.
3. **Provenance** — the event row names which alert matched which rule; the
   approval names the job and the workspace.
4. **Stale plan is refusal** — when the workspace serial moved after the plan,
   approving is refused with what moved it, which job, and what triggered it.
   The refusal deliberately does not spend the approval: discarding is a reject
   on the same link, and burning the token would leave neither option.
5. **A used or expired link is a terminal notice**, never an error.
6. **Mute always expires** — `silence add` requires `--for`, and `--global`
   additionally requires `--yes-really`.

An alert that resolves withdraws the approvals its firing raised and cancels
their jobs, before anything else looks at it: approving a restart for a host
that came back on its own is the one outcome nobody wants.

## Drift

A `tofu-plan` schedule enqueues an ordinary tofu job keyed `tofu:<workspace>`.
The driver runs `plan -detailed-exitcode`, and "changes present" arrives as a
plan-outcome job event rather than a failed job. A sweep turns each such job
into a `drift` event in the same pipeline, so a rule can gate an apply on drift
alone.

The event table is that sweep's own checkpoint: the drift event's dedup key
names the job that found the drift, so a job already reported is skipped without
a cursor to keep or state to lose on a restart.

## What is persisted

`event` (source, dedup key, normalized and raw bodies, matched rule, outcome,
reason, job, trace), `rule`, `rule_fire`, `silence`, `approval`. Migration 0005.
Both stored bodies go through the secrets redactor before the write: a webhook
body is attacker-influenced text landing in a durable table.
