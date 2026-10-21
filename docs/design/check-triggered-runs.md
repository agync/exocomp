---
type: Design
title: "Checks, network groups and check-triggered runs"
description: "Named probe-sets, named CIDR sets, and the governed playbook rule action that let a run trigger on a reachability or health transition."
status: stable
tags: [design, checks, reactive]
---

# Design — Checks, network groups, and check-triggered runs

## Status

Implemented (the shipped subset): netgroups and webhook peer stamping (§4.1,
§8.1), the check engine — `check_row`/`check_state`, the sweep with
transition-only emission, N-consecutive hysteresis, and cold-start seeding
(§4.2, §5) — and the governed `playbook` rule action (§7). The web ships
netgroup and check pages and a read-only rules page; rule mutation stays
CLI. The set-event host-set expansion (§7.4) was later built and then removed
by operator decision; see
[Decision 0019](../decisions/0019-aggregate-check-events-removed.md). Still not built: the
presence shim (§8.2), a web rule-builder, and run-secrets/vault on rules. Where
the shipped code diverges from this proposal, code wins and the correction is
recorded in `docs/log.md`.

---

## 1. Vision

Run a governed job not only on a wall clock but on a *state transition*:

- "Run the update playbook on my laptop when the laptop joins the home network —
  and not when it is outside the house."
- "Restart the service when its health endpoint goes bad."

Three new primitives deliver it:

1. **Checks** — a named, reusable probe set. `check add at-home --kind tcp
   --interval 5m --target laptop=192.168.1.50:22 --target nas=192.168.1.60:22`.
   A check answers "is this host reachable right now" for each target, and fires
   one event into the existing reactive pipeline when a target's answer flips.
2. **Network groups** — named CIDR sets, the operator's own vocabulary.
   `netgroup add home 192.168.0.0/16,10.0.0.0/8`. A group classifies an address
   (a probe target, or a webhook's peer) so a rule can ask
   `event.labels["netgroup"] == "home"` without the daemon knowing what "home"
   means.
3. **A governed `playbook` rule action** — a rule can launch a real ansible run
   (source checkout, inventory, run-secrets, sandbox, approval attribution)
   instead of only a fixed shell argv.

The trigger surface is then uniform: a **schedule stays a wall clock** (cron), and
a **check is an event source** a rule listens to. Two triggers for the same run =
a schedule row and a rule row, both converging on the one queue.

---

## 2. What already exists — and why the naive version is not enough

The reactive layer is a complete "event → CEL rule → guardrail → queue → job"
pipeline (`architecture/reactive.md`). A naive version of this feature works today:

```sh
# laptop-side: on link-up, POST to the generic hook
curl -X POST -H 'Content-Type: application/json' \
  http://<daemon>/hook/<generic-token> \
  -d '{"name":"host.up","host":"laptop","status":"up","subnet":"home"}'

exocomp rule add laptop-home \
  --source generic \
  --expr 'event.name == "host.up" && event.host == "laptop"' \
  --action shell -- /usr/local/bin/laptop-update.sh --cooldown 24h
```

`status: up` maps to `firing`; a later `down` maps to `resolved` and *withdraws*
the approvals its firing raised (`internal/reactive/engine.go:226`); dedup and
cooldown stop WiFi blips from hammering the run. What the shipped state cannot
do, and why:

| Gap | Why the naive version is wrong |
|---|---|
| **Location is client-asserted.** The laptop says "I'm home". A lying client, a misconfigured shim, or anyone holding the hook token can claim home from anywhere — and the daemon is reachable over tailnet, so "outside the house" does not even prevent the POST. | Location must be decided by the daemon, from where the signal physically arrived or from the address a probe actually reached. |
| **The only rule action that runs code is `shell`.** A rule's action is a fixed argv (`internal/reactive/action.go:69-84`); there is no governed playbook path. Operators would wrap `exocomp playbook run` in a script and call it from shell — the exact side door the one-queue invariant (AGENTS.md 5) exists to close. | A rule must be able to launch a governed run through the queue's own assembly path. |
| **No check exists.** Reachability is somebody else's job (a monitor posts it). | Checks make the daemon the observer, which is what makes the location signal trustworthy and the feature self-contained. |

**The precedent this is built on:** drift. A scheduled `tofu plan` whose exit
code says "changes present" becomes an `event` in the same pipeline through
`Engine.SweepDrift` (`internal/reactive/drift.go:18`), with `SourceDrift` a
non-webhook source (`internal/reactive/event.go:17`). A check sweep is the same
shape: the daemon observes, and observation becomes an event.

---

## 3. Core concepts and the two decisions that shape the design

### 3.1 Checks are named probe-sets; one event per transition

A check has a kind, an interval, and a set of targets. Each probe answers "is
this one target reachable". Nothing is emitted on a steady state — an event is
emitted only when a target's answer **flips** down→up (`firing`) or up→down
(`resolved`). Emitting only on transitions is what keeps dedup honest and gives
the resolve → withdraws-pending-approval semantics for free.

### 3.2 Reachability ≠ location — but the target address encodes location

A probe from the daemon answers "can *I* reach it", not "is it on the home
wifi". Over tailnet the laptop is reachable from anywhere, so a bare reachability
check cannot satisfy "not when outside the house".

The trick: **probe the host's home-LAN address, not a name.** The daemon runs on
the LAN. `tcp 192.168.1.50:22` answers only while the laptop is actually on that
LAN — away at a coffee shop its address is a different range and the home IP goes
silent. The *address* is the location. Asleep-at-home is handled for free: asleep
= no reply = `down`, so "update when it wakes up at home" is exactly the
down→up transition firing the rule. No client shim, no peer verdict, no
credentials on the laptop.

This is why checks are sufficient for the headline case, and why a netgroup is
an *optimisation* rather than a requirement: the operator already wrote the
location down by choosing `192.168.1.50` as the target. The netgroup adds the
operator's *name* for that fact (`home`), so the rule reads
`event.labels["netgroup"] == "home"` instead of hardcoding a prefix, and the same
group classifies webhook peers on the push side.

### 3.2.1 Preconditions — the two ways address=location fails silently

The mechanism rests on a stable, LAN-only address↔host binding. Two
preconditions must hold, or it fails open rather than failing closed:

1. **Static lease / reservation per target.** A bare `tcp 192.168.1.50:22`
   dial proves *something* answers on `:22` at `.50` — not that it is the
   laptop. If the laptop leaves and DHCP re-leases `.50` to a phone, the check
   reports "laptop home" for the phone. Targets must be pinned by static
   reservation (or the operator accepts a documented misattribution window).
   IPv6 SLAAC privacy addresses rotate the same way in reverse.
2. **No overlay route to the LAN range.** The whole premise is that `.50` goes
   silent when the laptop is away. A tailscale subnet router advertising
   `192.168.1.0/24` makes the daemon reach `.50` over tailnet from anywhere —
   yet reaching it still requires a live host answering at `.50`, which is real
   presence, not a false home; the residual is a different device answering at
   the same address, already covered by item 1 (T32). The daemon must reach
   check targets **only via the LAN interface**, never an overlay subnet route:
   a deployment precondition the check sweep cannot enforce from inside the
   container (it has no routing-table authority), documented as a trust
   boundary in §10.

Both are owned as stated preconditions, and both get a threat-model row.

### 3.3 The trusted-source line — what may shape a run's `--limit`

A webhook body is attacker-influenced text that lands in a durable table
(`engine.go:474`, `record` scrubs it). It must **never** shape argv. A check or
drift event, by contrast, is **produced by the daemon's own sweep code** — the
probe answered, the daemon wrote the event.

This is the dividing line that keeps a rule's run parameters **fixed at rule-add
time**: a `playbook` action's `--limit`/`--tags` are validated constants, never
event-derived (§7.2). It is also the boundary any *future* set-expansion would
have to respect — if a later phase lets an event's host set shape a run's
`--limit`, only daemon-produced sweep events may carry that set, and a webhook
event is refused at rule-add time (§7.4). Phase 2 ships without any such
expansion.

---

## 4. Data model

### 4.1 `netgroup` — migration `0029_netgroup.sql`

```sql
CREATE TABLE netgroup (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    prefixes   TEXT    NOT NULL,           -- comma-separated CIDRs, validated at write
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

Store rows, like sources and schedules: plain SQLite, backed up with the data
volume (AGENTS.md 2). Prefixes are validated at write time by
`netip.ParsePrefix` (the same parser `--hook-allow` uses, `ingress.go:96`), so a
row that reads is a row that parses.

### 4.2 `check_row` and `check_state` — migration `0030_check.sql`

`check` is a SQLite reserved word, so the table is named `check_row`; the
`check_state` FK references it.

```sql
CREATE TABLE check_row (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL CHECK (kind IN ('tcp', 'ping', 'http')),
    interval_s   INTEGER NOT NULL,
    targets_json TEXT    NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- Per-target transition state. Survives a restart so the sweep never
-- mistakes a rebooted daemon for a fresh down->up transition (§5.2).
CREATE TABLE check_state (
    check_id     INTEGER NOT NULL REFERENCES check_row (id) ON DELETE CASCADE,
    target_name  TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('up', 'down')),
    consecutive  INTEGER NOT NULL DEFAULT 0,
    emitted      TEXT    NOT NULL DEFAULT '' CHECK (emitted IN ('', 'up', 'down')),
    first_seen   INTEGER NOT NULL,
    last_change  INTEGER NOT NULL,
    PRIMARY KEY (check_id, target_name)
);
```

`targets_json` is one target per entry:

```json
[
  {"name": "laptop", "address": "192.168.1.50", "port": 22},
  {"name": "nas",    "address": "192.168.1.60", "port": 22},
  {"name": "web",    "url": "https://web.lan/health", "expect": 200}
]
```

A target is either `address[:port]` (tcp/ping) or a `url` (http, optional
`expect` status). The `name` is the event's `host` and the rule's vocabulary —
it does not have to be a DNS name, because the *address* is the load-bearing
field for location (§3.2).

`check_state` holds where each target's status currently stands. A missing row
IS the unobserved cold-start state; a probe that finds no row **seeds** it from
the first observation — `status=result, consecutive=1, emitted=result` —
**without emitting** (§5.2). `consecutive` carries the hysteresis counter and
`emitted` the last status that was emitted, so a steady state never emits and a
restart cannot read `up` as a fresh transition. `check list` reads this table
for its per-target last status. Rows are pruned with their check on delete.

An introduction migration carries a pinned in-place-upgrade test
(`TestMigration0030UpgradesFrom029`), per the migration discipline.

### 4.3 Rule action `playbook` — no migration

`action_kind` has no CHECK constraint (`0005_reactive.sql:14`), so a new action
kind is a pure Go change. The action args ride the existing `action_args_json`
column, exactly as `RunnerName` rides `args_json` on the job row (no migration,
`service/playbook.go:39`):

```go
// internal/store/rule.go — RuleAction extension
type RuleAction struct {
    Argv      []string `json:"argv,omitempty"`
    Workspace string   `json:"workspace,omitempty"`
    Timeout   Duration `json:"timeout,omitempty"`

    Source   string   `json:"source,omitempty"`     // registered source name
    Playbook string   `json:"playbook,omitempty"`   // path under the source checkout
    Limit    string   `json:"limit,omitempty"`      // fixed host pattern (validated)
    Tags     []string `json:"tags,omitempty"`
    Check    bool     `json:"check,omitempty"`
    Diff     bool     `json:"diff,omitempty"`
}
```

`Source`/`Playbook`/`Limit`/`Tags` are validated at `rule add` time (path exists
under the source checkout via `ResolveSourcePlaybook`, `service/source.go:49`;
limit/tags through the existing `AnsiblePassthrough` validation,
`service/ansible.go:35`), so a rule that would never run is refused at write time
— the same discipline `rule add` already applies to CEL compile.

### 4.4 An optional `hosts` field on events — removed

A set-event (one event carrying every present target, so one run can cover the
whole present subset) was proposed here and later built, then removed by
operator decision. Phase 2 ships per-target events only, and the normalized
`Event` (`event.go:35`) carries no host-set field. For the removal and its
rationale see
[Decision 0019](../decisions/0019-aggregate-check-events-removed.md).

---

## 5. Check semantics

### 5.1 Probe kinds

| Kind | Mechanism | Exit/result truth | Presence semantics |
|---|---|---|---|
| `tcp` | `net.DialTimeout` on `address:port` | dial succeeds | **Default presence probe.** `:22` answers "the OS is up" without the ICMP privilege story. Works against hosts that block ping. |
| `ping` | `ping` subprocess (`iputils` in the image), exit code only (AGENTS.md 3) | exit 0 | Same as tcp, for hosts that answer ICMP. Rootless constraint: unprivileged ICMP needs `net.ipv4.ping_group_range` to cover the daemon's gid. See §5.4. |
| `http` | `http.Client` with a short timeout | status in range (2xx/3xx) | **Liveness, not location.** The URL decides what is reachable; the body is never read. |

### 5.2 Transition-only events, with hysteresis and cold-start seeding

`Engine.SweepChecks` (a sibling of `SweepDrift`, called on the same
`--sweep-interval` tick, `serve.go:646-669`) runs each enabled check at its
interval and probes every target. Three rules bound what reaches the pipeline:

- **Hysteresis.** A transition is declared only after **N consecutive
  observations** of the new result (default 3 intervals), so a target on
  marginal wifi cannot flap up/down/up — each flap would otherwise withdraw and
  re-raise a pending approval, churning the events table. The run-side cooldown
  is not a substitute: it bounds how often a rule *fires*, not how often the
  pipeline churns.
- **Cold-start seeding.** A target with no `check_state` row is **observed and
  seeded without emitting**: its first probe sets the row and emits nothing, so
  a daemon restart does not read every up target as a fresh down→up transition
  and fire every presence rule. Only a change *from a seeded value* is a
  transition. A target that left and returned while the daemon was down is
  missed once (the restart observes `up` against a persisted `up`) — fail-safe,
  and the accepted price of never spurious-firing after a restart.
- **Transition-only emission.** An event is emitted only on a declared
  transition, never on a steady state. down→up: `{source: check, kind: check,
  name: <check>, host: <target>, status: firing, labels: {netgroup: <groups
  containing the target address>}}`, dedup key `check:<check>:<target>`.
  up→down: the same shape with `status: resolved` and the same dedup key, so a
  resolve finds and withdraws what its firing raised (`engine.go:226`) — the
  laptop leaving home cancels the pending approval its arrival raised. A steady
  state only updates `check_state`.

  Hysteresis applies to both directions, which means a departure withdraws a
  pending approval only after N consecutive `down` observations — a `~N·interval`
  lag on the resolve path, not on the fire path. That is the right trade: the
  `firing` event (and its run) is what has a real-time cost, and a resolve that
  lands a few intervals late only holds an approval a little longer; the approval
  still fails closed at its TTL if it truly was a flake. The lag should be stated
  in the docs when the feature ships.

### 5.3 Location classification

At emission, the sweep classifies each target's **address** against netgroup
prefixes and stamps `labels["netgroup"]`. A target address inside `home` is
`home`; one that is in no registered group is `away`. The rule filters on the
group name, never a prefix literal.

### 5.4 Rootless notes

The container image runs no sudo (AGENTS.md 6). `tcp` and `http` need no
privilege at all — which is why `tcp` is the default presence kind. `ping`
depends on `ping_group_range` covering the daemon's gid, and the image ships no
setuid or capability helper for it. `check add --kind ping` runs a loopback ping
and is refused loudly on a host that cannot send ICMP, the same
assert-don't-assume posture `sandbox.go Probe` holds.
The shipped container units set the range for the image's gid
([containers](../operations/containers.md#ping-checks)).

`http` records **status only, never the body** (AGENTS.md 4): a health endpoint's
response is not evidence this code should persist.

### 5.5 Dedup and cooldown granularity — per-rule for webhooks, per-target for checks

The guardrails run in ledger order (`guard.go:84`) and cooldown is **per rule**
today (`guard.go:182`): one `rule_fire` ledger, one budget. That is wrong for a
multi-target check: if `laptop` joins at 09:00 (rule fires, cooldown 24h) and
`pi` joins at 09:10, the second event is suppressed by the *rule's* cooldown and
`pi` never runs.

Recommendation: key the cooldown off the event's **dedup key** when the rule's
source is a sweep source, keeping **per-rule for webhooks — and the split is
deliberate, not arbitrary.** A webhook's dedup key is fingerprint-based
(`event.go:101`): distinct bodies get distinct keys. Uniform per-dedup-key
cooldown would hand a webhook that varies its body a *fresh budget on every
distinct body* — a complete bypass of the throttle. The sweep sources' dedup keys
(`check:<check>:<target>`) are a bounded, daemon-controlled set, so per-target is
safe there and true to the "each host's join gets its own slot" semantics.

This is a change to `guardCooldown`'s lookup (query `rule_fire` by
`rule_id + dedup_key` for sweep-sourced rules, `rule_id` for webhooks), a column
on `rule_fire`, and a documented carve-out — not a break in the order or the
ledger discipline.

---

## 6. The trigger model: a schedule stays cron; a check is a rule

"Give a job different triggers — time (cron), or one of these checks" is the
right mental model, but the mechanism should not make the *schedule* polymorphic.
Schedules keep their kinds (`shell`/`backup`/`tofu-plan`/`play`; the Go
constants at `store/schedule.go:15`, the value set enforced by the CHECK
constraints in migrations `0015`/`0020`); a check is a **pull event source** a
rule listens to, the same way drift is:

```sh
# cron: the schedule row fires an ansible job at 03:00 every day
exocomp schedule add nightly-update --cron '0 0 3 * * *' --kind play --source ops --playbook playbooks/update.yml --limit laptop

# check: the check sweep emits an event; the rule turns it into the same run
exocomp check add at-home --kind tcp --interval 5m \
  --target laptop=192.168.1.50:22 --target nas=192.168.1.60:22
exocomp rule add laptop-update-on-home \
  --source check \
  --expr 'event.name == "at-home" && event.labels["netgroup"] == "home"' \
  --action playbook --repo-source ops --playbook playbooks/update.yml --limit laptop \
  --cooldown 24h --daily-cap 1
```

Both produce the same `queue.Spec{Kind: ansible, ...}` and both go through the
same queue, guardrails, and approval machinery. A "job with two triggers" is a
schedule row **and** a rule row — the queue is the convergence point, so no new
scheduler machinery and no trigger column.

---

## 7. The governed `playbook` rule action

### 7.1 The seam — no import cycle, engine keeps owning the gate

`service` already imports `reactive` (`service/approval.go:6`), so `reactive`
cannot import `service`. The seam is an interface in `reactive`, resolved the
same way `Gate` already is (`engine.go:25`):

```go
// internal/reactive — the engine asks, the daemon answers
type PlaybookBuilder interface {
    BuildPlaybookSpec(ctx context.Context, ev Event, rule store.Rule) (queue.Spec, error)
}

// Options gains:
Playbooks PlaybookBuilder
```

The daemon's `serve` composition supplies an implementation backed by
`service.Service` (resolve source checkout, validate the playbook path and
limit/tags through `AnsiblePassthrough`, assemble `queue.Spec`). `buildSpec`
(`action.go:69`) gains the `playbook` branch; everything after it — enqueue,
gate, approval, `RecordRuleFire`, the trace — is untouched. The engine still
owns what reaches the queue; the builder only shapes the spec.

### 7.2 Limit assembly — fixed patterns only, required

`--limit` on a `playbook` action is a **required, fixed host pattern validated at
rule-add time** — an event value never shapes it (consistent with
`service/ansible.go`). It is required because an un-limited join rule would run
the playbook against every host the checkout's inventory names, a blast radius
the queue guardrails do not bound: default-deny, matching the repo's posture.
In the shipped phase a per-target event means exactly one thing to run:
`event.host`; the rule pins its own target(s) at add time. Nothing expands an
event value into argv; the set-event expansion was proposed for §7.4 and is now
removed (Decision 0019).

The CLI flag is `--repo-source` for the playbook's registered source, because
`--source` is already taken by a rule's event-source filter (`cmd/exocomp/rule.go:133`);
the store field stays `source` on `RuleAction`.

### 7.3 Approval attribution

A `playbook` action is not destructive by default (the way `tofu-apply` is,
`action.go:41`), but a rule can `--require-approval` it, and the approval then
carries the run's job id through the existing `gateAction` path
(`engine.go:337`). The resolve → withdraw path (§5.2) covers a check that clears
while an approval is pending.

### 7.4 Removed: the set-event host-set expansion

The headline case is per-target — `event.host` is the laptop, the run covers the
laptop. A set model (one event per transition carrying the full present set,
expanded into a fixed host pattern) served the narrower "run over exactly the
present subset" need. It was designed here, later built, and then removed by
operator decision: per-target rules cover the operator's cases, and the set
model's extra field plus expansion and validation path were not wanted. See
[Decision 0019](../decisions/0019-aggregate-check-events-removed.md).

---

## 8. The push side: peer stamping, then the presence shim

Checks cover hosts the daemon can probe. Two things remain:

1. **Peer stamping (cheap, do first) — a network-path signal, not a location
   verdict.** The ingress already computes the peer address for `--hook-allow`
   (`ingress.go:119`). Stamping every normalized event with `labels["peer_ip"]`
   and `labels["peer_netgroup"]` (the first registered group containing the peer,
   else `away`) costs one flag-free lookup and hardens the *existing* generic
   hook. Be precise about what it answers: the peer is `r.RemoteAddr`
   (`ingress.go:123`), so over tailnet it is the CGNAT `100.64.0.0/10` address —
   a home netgroup of LAN prefixes marks **every** tailnet-delivered hook away,
   including a client physically at home but connected over the overlay. That is
   fail-safe for a "refuse from outside" rule, and it is the honest framing: the
   signal is *did this arrive on the LAN or over the overlay*, not *is the
   sender home*. The verdict is unforgeable by the client (it comes from where
   the connection physically arrived), which is the property that matters.
2. **Presence shim (phase 3, optional).** For hosts the daemon cannot probe
   (NAT'd clients, IoT behind a guest network), ship a small NetworkManager /
   `network-online` script under `packaging/presence/` that HMAC-signs
   `{"host","status":"up|down"}` and posts it to a `presence` webhook source
   (token derived `Derive("webhook-hmac", "presence")`, the same bearer model as
   every hook, `ingress.go:70`), with a client-side debounce so a walk-in does
   not fire mid-sentence. The peer stamping above is what keeps "outside the
   house" from firing even when the shim runs over tailnet.

Checks are the primary mechanism; the shim is the escape hatch for what cannot
be probed.

---

## 9. CLI, API, and web surface

```sh
exocomp netgroup add home 192.168.0.0/16,10.0.0.0/8
exocomp netgroup list
exocomp netgroup rm home

exocomp check add at-home --kind tcp --interval 5m --target laptop=192.168.1.50:22
exocomp check add web-health --kind http --interval 1m --target web=https://web.lan/health --expect 200
exocomp check list --output json      # per-target last status, last transition
exocomp check enable|disable|rm <name>

exocomp rule add <name> --source check --expr '...' \
  --action playbook --repo-source ops --playbook playbooks/update.yml \
  [--limit <pattern>] [--tags ...] [--check] [--diff] \
  --cooldown 24h --daily-cap 1
```

`rule add` learns the `playbook` action and its flags (`cmd/exocomp/rule.go:39`);
`rule list` renders it (`rule.go:201`). `wire.RuleView` (`wire/schema.go:395`)
gains the action fields. The web surface gains netgroup and check pages (rows +
modals, the same chrome the sources/schedules pages use); rules are read-only on
the web — mutation stays CLI, so there is no in-browser rule builder. `serve`
prints check sweep status beside the drift line (`serve.go:655`).

---

## 10. Security and invariants

Threat-model additions (new T-rows under the reactive section, in the existing
sequential catalog):

| id | threat | mitigation |
|---|---|---|
| T30 | A spoofed home-presence claim (webhook or a forged peer) fires a run from outside the house. | Peer stamping reads the physical connection (`peer_netgroup`), never a claim; a check's location is the address the daemon itself probed (§3.2 preconditions still apply). |
| T31 | A check flood re-arms a rule past its budget (an interval set too short, or a target flapping). | Transition-only emission with N-consecutive hysteresis; per-target dedup and cooldown (§5.5); the daily cap throttles rather than disables. |
| T32 | DHCP re-leases a target's home address to a different device, so a check reports the wrong host as present. | Stated precondition (§3.2.1): targets are pinned by static reservation or fixed lease; the netgroup classifies the address, never the host. |
| T33 | An overlay subnet route (tailscale advertising the home LAN) keeps a checked address reachable from anywhere while an overlay host answers at that address. | An overlay route still requires a live host at the probed address (real presence); the residual is address-uniqueness (T32). LAN-only routing is a documented trust boundary — the sweep has no routing-table authority from inside the container. |
| T34 | A daemon restart reads every up target as a fresh down→up transition and fires every presence rule. | Cold-start seeding writes `check_state` and emits nothing on first observation; only a change from a seeded value is a transition (§5.2). |
| T35 | An http check persists a health endpoint's body (a secret, a stack trace). | `http` records status only; the body is never read into the event or a ledger. |
| T36 | An operator-created http check targets the daemon's own loopback listeners or the cloud metadata service, so the check's result depends on a service the operator did not mean to reach. | The http probe dials through a `net.Dialer.Control` hook that refuses loopback, link-local unicast (covers `169.254.169.254`), unspecified, and multicast addresses at the connected IP, scoped so RFC1918/ULA LAN targets are still allowed; a refused address observes down. |

The canonical rows, with the full mitigation text, live in
[`security/threat-model.md`](../security/threat-model.md).

Invariant mapping:

- **1** — one container, no new daemon or store dependency.
- **2** — netgroups and checks are plain SQLite rows (migrations 0029/0030).
- **3** — probe results are exit-code / dial-result truth, never parsed output.
- **4** — no probe body, peer verdict, or run-secret value reaches logs or ledgers.
- **5** — a check becomes an event; a rule becomes a job; every path still
  crosses the one queue.
- **6** — `tcp` is the default presence probe; `ping` is refused on a host that
  cannot enforce it without sudo.
- **7** — no registry, vendor, or external feed is on the path; a check observes
  the LAN the daemon already sits on.
- **8** — this document, the bundle index, and `log.md` move with the change.

---

## 11. Sequencing

Each phase is a self-contained unit; Phase 1 pays for itself even if 2–3 never
ship.

- **Phase 1 — netgroups + peer stamping.** `0029_netgroup.sql`, the `netgroup`
  verbs, and the ingress stamping of `peer_ip`/`peer_netgroup`. Harden the
  existing generic-hook path and give rules the operator's own vocabulary.
  Small, isolated, no sweep machinery.
- **Phase 2 — checks + the governed playbook action.** `0030_check.sql` (the
  `check_row` and `check_state` tables), the check store/CLI/API/web verbs,
  `SweepChecks` with hysteresis and cold-start seeding, the `check` source,
  per-target cooldown, the `playbook` rule action and its `reactive`-side builder
  seam. Per-target events only. This is the load-bearing phase: it delivers the
  laptop use case end to end with no client software.
- **Phase 3 — presence shim (optional).** The `presence` webhook source, the
  shipped shim under `packaging/presence/`, debounce, and the HMAC-signing
  discipline. Only for hosts a probe cannot reach.
- **Phase 4 — set-event (built then removed).** A set event carrying the full
  present set and its host-pattern expansion was proposed here, later built, and
  removed by operator decision (Decision 0019). The headline case does not need it
  (§7.4).

---

## Open questions

- **Hysteresis N and granularity** (§5.2): resolved. N is fixed at 3
  consecutive observations, not operator-tunable, and a missed interval is a
  skip, not an observation. Granularity is per-target for the check source,
  settled with the cooldown keying below.
- **`check_state` retention** (§4.2): resolved. Shipped as last-status-only —
  rows are pruned with the check on delete, and the events table already
  records every transition, so no separate history table.
- **HTTP check on a tailnet URL**: resolved. An http target's URL resolves
  off-LAN and carries no netgroup label; its liveness is not a location
  verdict, which a "trigger on bad health" rule does not need.
- **Cooldown keying** (§5.5): resolved. Migration `0031_rule_fire_dedup.sql`
  adds the `rule_fire.dedup_key` column; sweep-source rules key cooldown off
  the event's dedup key (`check:<check>:<target>`), webhooks stay per-rule.
- **Set-event host-set expansion** (§7.4): designed, built, then removed by
  operator decision in favor of per-target events (Decision 0019).
