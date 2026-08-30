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

Every normalized event is stamped with the network path it arrived on before
the pipeline evaluates it: `labels["peer_ip"]` (the peer address, unmapped)
and `labels["peer_netgroup"]` — the registered netgroup whose prefix most
specifically contains the peer, or `away` when none does. It is a
network-path signal, not a location verdict: over tailnet the peer is the
CGNAT `100.64.0.0/10` address, so a home netgroup of LAN prefixes marks every
overlay-delivered hook away — the fail-safe direction for a "refuse from
outside" rule. The value comes from where the connection physically arrived,
so the sender cannot forge it.

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
(no job at all), and `playbook` — a rule launches a real ansible run through
the queue's own assembly path (the engine asks the daemon's `PlaybookBuilder`
to resolve a registered source's checkout) instead of only a fixed shell
argv. The playbook's `--limit` is a required, fixed pattern validated at
rule-add time — an event value never shapes a run's argv.

## Guardrails, in ledger order

They run in this order and the order is the contract: a silenced host must not
spend a rule's daily cap, and a duplicate must not reset a cooldown.

| Step | What it asks | Reason string it writes |
|---|---|---|
| silence | does an active silence cover this event? | `silenced: host web-01 until 14:32 (12 swallowed)` |
| dedup | has this condition already been decided inside the window? | `duplicate of event 41 within 5m` |
| cooldown | did this rule fire too recently? | `cooldown 6h, next eligible 14:32` |
| daily cap | has this rule spent its budget? | `daily cap 5 reached; rule restart throttled but still armed` |
| approval | is the action destructive, or does the rule require one? | `approval 3 pending, expires 14:32` |

Silences are rows with a mandatory `expires_at`; there is no never-expires form,
and expiry is decided at read time so nothing has to sweep for one to lift. The
global scope is the reactive kill switch — one row, not a separate flag, so
pausing everything obeys the same mandatory expiry as muting one alert.

Cooldowns and caps read `rule_fire` rows rather than counters in memory, so a
crash-loop restart does not hand a rule a fresh budget. A cap **throttles** the
rule — it withholds the action but keeps the rule armed — and pages at max
priority, once per rolling window. Disabling the rule instead would let a flood
of matching events blind a security rule: it would go dark until a human
re-enabled it, which is exactly what an attacker floods to achieve.
Throttling keeps the rule evaluating, so every over-cap event stays visible as a
suppressed row naming the rule, the budget recovers on its own when the rolling
window clears, and the loud page is throttled to one per window so the flood
cannot turn into a notification storm.

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
that came back on its own is the one outcome nobody wants. A bare resolve
carries no approval credential, so its authority to cancel is the rule that
raised the approval: the resolve must come from the same source, be admitted by
that rule's source filter, and match the rule when its status is read as firing.
A forged resolve that shares only the dedup key — stripped of the labels the
rule fired on — matches no rule and cancels nothing; the approval then fails
closed at its TTL.

### Crash recovery of the gate

Arming a gate is two writes — the job row in `awaiting_approval`, then its
approval — and deciding one is two more — the approval to a terminal state, then
the job out of `awaiting_approval`. A crash between either pair strands a job: a
decided approval whose release never landed leaves the job stuck
`awaiting_approval` forever, and a job whose approval was never written can be
decided by no one. `ReconcileNonTerminal` closes both by construction. It is one
idle-tick pass over every non-terminal job that reconciles each to what its
approval says it should be: a settled approval finishes its lost transition
(approved → `queued`, otherwise → `cancelled`) regardless of who owns the job,
because the human already answered; a gated job with no approval at all is
retired to `interrupted` once its creator process is gone — fail-closed and
re-drivable, never auto-released, because a destructive apply must not run
without a decision. A row whose gate is still pending is the gate working and is
left untouched. Every move is a guarded state transition, so the pass is
idempotent and races the live decision path safely.

## Drift

A `tofu-plan` schedule enqueues an ordinary tofu job keyed `tofu:<workspace>`.
The driver runs `plan -detailed-exitcode`, and "changes present" arrives as a
plan-outcome job event rather than a failed job. A sweep turns each such job
into a `drift` event in the same pipeline, so a rule can gate an apply on drift
alone.

A `play --check` schedule is the config half, swept by the same loop. The driver
attaches the check-mode outcome — per-host counts of tasks that would change — to
a job event, and a check whose total is above zero becomes an `AnsibleDrift`
event beside the tofu `TofuDrift`. Both carry `source: drift`, so one rule's
source filter admits either.

The event table is that sweep's own checkpoint: the drift event's dedup key
names the job that found the drift, so a job already reported is skipped without
a cursor to keep or state to lose on a restart.

A drift subject is what the incident is opened against, and it differs by plane.
Tofu's is the workspace, read off the job's `workspace:<name>` runbook. Ansible
has no equivalent registered noun on the job row, so its subject is
`<runbook>/<playbook base name>`: the runbook is a play schedule's own name, and
the playbook is what keeps the manual `playbook run` verb — which stamps every
job with the runbook `playbook` — from collapsing every hand-run check into one
incident. Only the base name travels; the rest of the path moves between runs,
and a subject that moves cannot close what it opened.

The close is symmetric on both planes. A later clean dry run for the same
subject pages once with the opening page's exact `Source`/`Host`, so a GoAlert
transport closes the incident rather than opening a second one, and writes a
clear checkpoint keyed by the OPENING job — one existence check answers "is this
subject's latest drift still open", and a repeat sweep closes nothing. Only a
dry run that finished and reached every host clears an incident: a failed check
never enters the sweep, and a degraded one (tolerated unreachable hosts) saw
nothing at all on the hosts it missed, so it leaves the incident open.

## Checks

A check is a named probe set — a kind, an interval, and targets — whose
answers become events in this same pipeline. `tcp` dials `address:port` (the
address is the location: probe the home-LAN address a host is only reachable
at when it is home), `ping` runs one `ping -c1 -W2` subprocess and reads the
exit code only, `http` GETs a URL and records the status line — the body is
never read (AGENTS.md 4). `SweepChecks` runs each enabled check on its
interval beside `SweepDrift` on the same `--sweep-interval` tick, and `check`
is a pull source (`SourceCheck`), not a webhook: nothing posts it.

Three rules bound what reaches the pipeline:

- **Transition-only.** An event is emitted only when a target's answer flips —
  down→up fires, up→down resolves — never on a steady state. The dedup key is
  `check:<check>:<target>`, so a resolve finds and withdraws what its firing
  raised: the laptop leaving home cancels the approval its arrival raised.
- **Hysteresis.** A flip becomes a transition only after N consecutive
  observations of the new result (default 3), in both directions, so a target
  on marginal wifi cannot flap a pending approval. The lag applies to the
  resolve path as much as the fire path; an approval held a few intervals
  longer still fails closed at its TTL.
- **Cold-start seeding.** A target with no state row is observed and seeded
  without emitting, with `emitted` seeded to the observed result — so a
  daemon restart does not read every up target as a fresh down→up transition.
  Only a change from a seeded value is an event; a target that left and
  returned while the daemon was down is missed once, the accepted fail-safe
  price.

At emission a tcp/ping target's event carries `labels["netgroup"]` — the
registered group whose prefix most specifically contains the address, or
`away` — so a rule can gate on the operator's own vocabulary instead of a
prefix literal. An http target's URL resolves off-LAN, so it carries no
group; its liveness is not a location verdict.

Guardrails key the same way the sources do, with one deliberate split: the
cooldown ledger is read **per dedup key** for the check source — each target
of a multi-target check gets its own slot, so `pi` joining ten minutes after
`laptop` is not suppressed by `laptop`'s cooldown — and **per rule** for
every other source. A webhook's dedup key is fingerprint-based: per-key
cooldown would hand a varying body a fresh budget on every distinct body, a
complete bypass of the throttle. The check source's keys are a bounded,
daemon-controlled set, so per-target is safe there. The daily cap stays
per-rule for every source.

## Job failures

A second sweep runs on the same tick: it lists recently-finished jobs in a
terminal failure state (`failed`, `timed_out`) and pages the operator once for
each one it has not already paged on, at high priority, attributed by runbook
and workspace so a scoped mute can hold it and carrying the run's last error.
Without it a failed scheduled converge or backup runs completely silent. A
failed workflow step surfaces here through its own failed job, so the one scan
covers both; a blocked step never ran, and the earlier failed step that blocked
it is the one that pages.

Its idempotency is the drift sweep's, unchanged: the page leaves a
`job-outcome` event whose dedup key names the job, so `CountEventsByDedupKey`
skips a job already paged on across repeated sweeps and across a restart — the
same event-table checkpoint, not a weaker second mechanism. The row is written
after the page, so a crash between the two re-pages once rather than losing the
alert.

## What is persisted

`event` (source, dedup key, normalized and raw bodies, matched rule, outcome,
reason, job, trace), `rule`, `rule_fire`, `silence`, `approval`. Migration 0005.
Both stored bodies go through the secrets redactor before the write: a webhook
body is attacker-influenced text landing in a durable table.
