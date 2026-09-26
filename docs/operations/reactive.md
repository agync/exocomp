# Reacting to alerts

How to point a monitor at exocomp, write a rule, and answer the phone. The
mechanism behind all of it is in
[architecture/reactive.md](../architecture/reactive.md).

## Wiring a sender

`exocomp serve` prints one hook URL per source at startup:

```
state backend listening on http://127.0.0.1:41337
hook alertmanager  http://127.0.0.1:41337/hook/a3f19c04be2d7e5b8c40
hook healthchecks  http://127.0.0.1:41337/hook/71bd0e6ac8f4529d31ae
hook generic       http://127.0.0.1:41337/hook/0c5e83fb47a19d2e6b70
```

The token in the path is derived from the envelope, so it survives a restore and
is never written down anywhere but the log line above. Treat the URL as a
credential.

In Alertmanager:

```yaml
receivers:
  - name: exocomp
    webhook_configs:
      - url: http://<host>:<port>/hook/a3f19c04be2d7e5b8c40
        send_resolved: true
```

`send_resolved: true` matters: a resolve is what withdraws a pending approval.

Anything else posts JSON to the `generic` hook. The body must be an object with
a `name`, `alertname`, `title`, or `event` key; every top-level scalar becomes a
label a rule can read. A sender that can compute an HMAC should send
`X-Exocomp-Signature: sha256=<hex of HMAC-SHA256(body)>` under the same subkey —
when the header is present it must verify.

Flags: `--hook-allow 10.0.0.0/8,192.168.0.0/16` restricts senders; `--public-url
https://exocomp.lan` sets what decision links point at when the daemon sits
behind a reverse proxy; `--dedup-window` and `--approval-ttl` set the two
windows; `--sweep-interval` (default 30s) sets how often TTLs are enforced,
drift is collected, and check probes are due; `--check-canary <ip:port>` names a
reachability witness the check sweep dials before declaring targets down (empty
disables; see [Checks as an event](#checks-as-an-event)).

## Push

Approvals reach a phone through ntfy, configured from sealed secrets:

```sh
printf 'https://push.lan'  | exocomp secrets add ntfy/url   --kind url
printf 'exocomp-approvals' | exocomp secrets add ntfy/topic --kind topic
printf 'tk_...'            | exocomp secrets add ntfy/token --kind token
```

A bare topic on the public `ntfy.sh` is **refused**, by name, at startup: an
approval URL is a bearer capability to run playbooks against the fleet, and a
public topic is readable and publishable by anyone who guesses it. Self-host, or
seal an access token. Without any of these the daemon warns once and runs with a
silent phone.

A GoAlert Generic API integration is a second transport, sealed as `goalert/url`
and `goalert/key`; both fan out together. See
[notifications](notifications.md#the-push-transport) for both and for the
job-failure paging the same sweep does.

## Rules

```sh
exocomp rule add restart-exporter \
  --expr 'event.name == "NodeExporterDown" && event.labels["severity"] == "critical"' \
  --action shell --cooldown 1h --daily-cap 3 \
  -- /usr/local/bin/restart-exporter.sh

exocomp rule add apply-drift \
  --expr 'event.name == "TofuDrift" && event.labels["workspace"] == "prod"' \
  --action tofu-apply --workspace prod
```

The expression is compiled before the row is stored, so a rule that would never
run is refused here rather than logged on every event. `event` carries `source`,
`kind`, `name`, `status`, `severity`, `host`, `instance`, `fingerprint`,
`summary`, `labels`, and `annotations`.

`rule list` shows each rule's guardrails and why it is gated. A rule that hits
its daily cap is throttled, not disabled: its actions are withheld and it pages
at max priority, but it stays armed and keeps evaluating, and the budget recovers
on its own once the rolling 24h window clears — a flood cannot blind it into a
state that needs `rule enable` by hand. (`rule enable`/`rule disable` still work
for a rule you turn off yourself.)

`tofu-apply` is approval-gated whatever `--require-approval` says.

## Silences

```sh
exocomp silence add --alert DiskFull --for 2h --reason "replacing the disk"
exocomp silence add --host web-01 --for 45m
exocomp silence add --global --for 90m --yes-really --reason "rack maintenance"
```

`--for` is mandatory at every scope. The global scope is the kill switch and
must be confirmed: it swallows every event from every source. Confirm it with
the global `--assume-yes` (`-y`); `--yes-really` still works as a deprecated
alias. On a non-interactive session (`--no-input`, or a piped stdin) an
unconfirmed global silence is refused with an actionable error rather than
waiting on input. The kill switch does **not** stop running jobs finishing and
it does **not** stop schedules firing — it stops events becoming new jobs.
`silence list` shows what is active and how long is left; `silence rm <id>`
lifts one early.

## Answering

```sh
exocomp approval list
exocomp approval link 3      # prints the two decision URLs
```

`serve --public-url` is what the daemon's own push notifications point at, but
that value lives only in the running daemon's memory — `approval link` cannot
read it back, so it defaults to the recorded loopback address, unreachable off
the box. Behind a reverse proxy, pass the same value with `approval link 3
--public-url https://exocomp.lan` (or export `EXOCOMP_PUBLIC_URL` once) so the
printed link is one you can actually open.

The links are single-use capabilities; printing one is handing it out. Tapping
either from a phone, or `curl`-ing it, is the whole flow. A used or expired link
answers with a terminal notice rather than an error, and an approval whose TTL
ran out has already rejected its job — an unreachable operator cannot cause
anything to run.

Approving an apply whose workspace moved since the plan is refused, with what
moved it and which job did. Re-plan, or reject to discard.

## Why didn't it fire?

```sh
exocomp event list --limit 20
exocomp event list --outcome suppressed
exocomp event explain 41
```

Every received event has a row, including the ones nothing matched. `explain`
prints the per-condition trace exactly as it was recorded — not recomputed,
because the ledgers it consulted have moved on since.

## Drift as an event

```sh
exocomp schedule add drift-prod --cron '0 0 * * * *' --kind tofu-plan --target prod
```

The fire is an ordinary `tofu plan` job keyed `tofu:prod`. When it finds
changes, the daemon's sweep turns that into a `drift` event, which a rule can
turn into an approval-gated apply.

The same sweep also pages once for any job that ends in a terminal failure, so a
failed scheduled converge or backup is no longer silent. The page is idempotent
over the same event-table checkpoint the drift sweep uses.

## Netgroups

```sh
exocomp netgroup add home 192.168.0.0/16,10.0.0.0/8
exocomp netgroup add home-except-guest 192.168.0.0/16,!192.168.5.0/24
exocomp netgroup update home-except-guest 10.0.0.0/8
exocomp netgroup show 192.168.5.10     # what classifies this address
exocomp netgroup usage                 # which checks and peers land where
exocomp netgroup list
exocomp netgroup rm home-except-guest
```

A netgroup names a set of CIDRs with the operator's own vocabulary. It
classifies two things: every webhook's peer and every check target's address.
Both are stamped before the pipeline evaluates the event, so a rule reads
`event.labels["netgroup"] == "home"` instead of a prefix literal. The webhook
stamp also carries `peer_ip` and `peer_netgroup` (the explicit network-path
name); `netgroup` and `peer_netgroup` are always equal for one peer — a
network-path signal, not a location verdict: over tailnet the peer is the
CGNAT address, so a home netgroup of LAN prefixes marks an overlay-delivered
hook away, the fail-safe direction for a "refuse from outside" rule.

A CIDR prefixed with `!` is an exclusion: an address inside any group's exclude
classifies to the miss (`away`) even when an include also covers it, so
`192.168.0.0/16,!192.168.5.0/24` expresses "the LAN except the guest VLAN" as
one group. `netgroup show <addr>` prints the group and the specific prefix that
classifies one address, or `away`; `netgroup usage [<name>]` is the read-only
report of which check targets (`check_state.netgroup`) and which observed
webhook peers (`peer_netgroup`) currently land in each group. `netgroup update
<name> <cidr[,...]>` replaces a group's set in place without disturbing stored
`check_state`. A group may not be named `away`: it is byte-identical to the
miss label.

## Checks as an event

```sh
exocomp check add at-home --kind tcp --interval 5m \
  --target laptop=192.168.1.50:22 --target nas=192.168.1.60:22
exocomp check list                 # per-target status, netgroup and duration
exocomp check list --netgroup home # only checks with a target stored in "home"
exocomp check list --output json   # the same per-target state, machine-readable
exocomp check probe at-home        # run the probes now, writing nothing
exocomp check test --kind tls --target web=web.lan:443 --min-days 21   # dry run
exocomp check snooze at-home --for 4h   # suppress transitions until the deadline
exocomp check unsnooze at-home
exocomp check disable at-home
exocomp check enable at-home
exocomp check rm at-home
```

A check is a probe set the daemon sweeps on the same tick as drift. Kinds:
`tcp` dials `ip:port` (the address is the location — probe the home-LAN
address a host is only reachable at when it is home), `ping` runs a `ping`
subprocess (refused at add on a host that cannot actually send ICMP), `http`
reads the status line and never the body, `dns` resolves a name (optionally
requiring one expected address), `tls` handshakes `host:port` and asserts the
leaf certificate is at least `--min-days` from expiry, and `metric` reads a
numeric body and asserts it satisfies an operator against a value. Checks and
the verify gates in a workflow run the same probe implementation
(`internal/probe`); a check refuses the loopback, link-local, unspecified, and
multicast dial classes a gate allows, and a check's default http acceptance is
2xx/3xx where a gate's is 2xx only.

`check add` and `check update` carry the per-kind config as flags: `--min-days`
(tls), `--expect-addr` (dns), and `--op`/`--value` (metric, both required). A
flag that does not belong to the check's kind is refused rather than dropped,
and the stored kind is never updatable. `check probe <name> [--target <t>]` runs
a stored check's targets immediately with the sweep's options and prints each
target's status, duration, location, and failure detail: it writes no state and
emits no event, so a check is debugged without waiting an interval. `check test
--kind <kind> --target <name=spec>` validates and runs a one-off target set
without storing a check, the dry run that catches a wrong target or threshold
before `check add`.

An event fires only on a declared transition (down→up fires, up→down
resolves) after 3 consecutive observations. The 3 is fixed — not
operator-tunable — and a missed interval is a skip, not an observation, so a
flaky target cannot nurse the counter toward a false transition. Hysteresis
applies to both directions: a departure withdraws a pending approval only
after 3 consecutive down observations, a ~N·interval lag on the resolve path,
not on the fire path. A check's interval is floored at the sweep tick:
whatever `--interval` says, a probe runs no sooner than the tick — the 30s
default `--sweep-interval` at worst. A restart never spurious-fires: a target
with no state row is seeded from its first probe without emitting, and the
price is fail-safe — a host already home at daemon start is missed once,
never read as a fresh down→up transition after a reboot.

Per-target transition state lives in a persisted `check_state` row — the
current `status`, the `consecutive` counter, the last `emitted` status,
`last_change`, the classified `netgroup`, and the last probe's
`last_duration_ms`. It survives a restart, which is exactly why a reboot cannot
read `up` as a fresh transition; the `/checks` page's "changed N ago" line
reads `last_change` off that row. `check list` renders the same state — the
text table's STATUS, NETGROUP, and DURATION columns, `--output json`, and the
`/checks` page all carry it. `check list --netgroup <name>` narrows the listing
to checks with at least one target whose stored netgroup is `<name>`, reading
the persisted classification rather than re-classifying. A declared transition's
event also carries the probe's measured duration as `labels["duration_ms"]`, so
a rule can act on "up but slow" (for example
`int(event.labels["duration_ms"]) > 2000`) without a second probe.

The sweep classifies each probed address once, at observation time, and stores
the result beside the probe's duration in milliseconds. The wire projections —
`check list --output json` and `/api/v1/checks` — read those stored values, so
an http URL target (no address) carries no `netgroup` and a target that is up
but slow is visible without a second probe. The `/checks` page reads the same
stored values for an observed target (a never-probed cold-start tile falls back
to a live classification, so it still names where it will be probed), renders
the last probe duration as dim meta, and badges a snoozed check with the time
left.

A target that is DOWN while classified into a registered (non-away) netgroup is
the signal the location trick exists to give: it should be on the LAN and is
not. The `/checks` page marks it distinctly from an ordinary roaming down — the
card counts "at home" and the tile reads "down while home — expected reachable"
— while a down-and-away laptop stays quiet.

A check-level snooze (`check_row.snoozed_until`) keeps probing and advancing
state but emits no transition until it expires; the expiry is mandatory and the
snooze lapses on its own. Set it with `exocomp check snooze <name> --for`, lift
it early with `exocomp check unsnooze <name>`; `check list` and the `/checks`
page show an active snooze and its remaining time.
`--check-canary <ip:port>` gates the other way: when a target reads down and
the canary is also unreachable, that check's whole sweep is a missed
observation — no state advances, nothing emits — so the daemon's own uplink
loss cannot page for every target at once. The canary is a bare dial; empty
disables it.

Every dialing check kind refuses the addresses a check must not reach. `tcp`,
`ping`, and `tls` apply the same guard the `http` client already did: loopback,
link-local unicast (covering the cloud metadata address), unspecified, and
multicast are refused at the connected IP, so a check against a refused address
observes down. A literal `tcp`/`ping` address is refused before the dial;
a `tls` hostname is refused at the connected IP, and a `tls` address with no
host (`:443`) is refused at validation rather than dialing the local host. The
guard applies to the sweep and to `check probe` alike, so a loopback `tcp`
target reads down on both. A gate's probes set `AllowLoopback`, so a workflow
step beside the host it verifies is unaffected. Only an address literal is a
location: `http`, `dns`, `tls`, and `metric` targets carry no netgroup label,
because a URL, a hostname, or a `host:port` is not a location verdict.

> **Before you rely on this.** The address-is-location trick holds only while
> two preconditions do, and both fail open:
>
> 1. **Static lease or reservation per target.** `tcp 192.168.1.50:22`
>    proves something answers on `:22` at `.50`, not that it is the laptop;
>    if DHCP re-leases `.50` to another device, the check reports that device
>    home. Pin each target by static reservation, or accept a documented
>    misattribution window.
> 2. **No overlay route to the home LAN.** A tailscale subnet router
>    advertising the LAN keeps a probed address reachable from anywhere, so
>    "not when outside the house" fails open. Check targets must be reachable
>    only via the LAN interface.
>
> Both are deployment preconditions the sweep cannot enforce from inside the
> container; see
> [design/check-triggered-runs.md §3.2.1](../design/check-triggered-runs.md).

A rule turns the event into a run the same way it turns any event into a
run:

```sh
exocomp rule add laptop-update-on-home \
  --source check \
  --expr 'event.name == "at-home" && event.labels["netgroup"] == "home"' \
  --action playbook --repo-source ops --playbook playbooks/update.yml \
  --limit laptop --cooldown 24h --daily-cap 1
```

The `playbook` action needs `--repo-source` and `--limit` (default-deny: an
un-limited rule would run against every host the inventory names). A check
rule's cooldown keys off `check:<check>:<target>`, so each target of a
multi-target check gets its own slot while webhook rules stay per-rule.

The web surfaces it across the UI. **Netgroups** is a tab on the **Checks**
page (the standalone `/netgroups` page now 302s to `/checks?tab=netgroups`),
and the API reads them at `/api/v1/netgroups` and `/api/v1/checks`; the web
has a read-only **Rules** page — rule mutation stays CLI. The checks page
renders each target's location as prose (`address is on home` / `not on any
netgroup (away)`), the last probe duration, and how long ago its status last
changed (`3h ago`, from `check_state.last_change`, or a cold-start
UNOBSERVED), and its edit dialog re-points a check in place — the kind fixed,
a kept target name keeping its state; the dashboard's
Checks card shows the registered total and how many targets the sweep
observes up; a job detail line "fired by rule <name>" links to `/rules`
and, when on record, the event that raised the run; and the events page
filters by `?source=`, one entry per pipeline (alertmanager, healthchecks,
generic, drift, check).

## Probe config as code

```sh
exocomp check export > checks.yaml          # YAML by default; --output json
exocomp netgroup export > netgroups.yaml
exocomp check apply -f checks.yaml          # create/update, idempotent by name
exocomp netgroup apply -f netgroups.yaml
exocomp check apply -f checks.yaml --prune --yes-really
git -C /srv/ops add checks.yaml && git commit -m 'checks: pin the estate'
```

`check export` and `netgroup export` are the same document `apply` reads, so a
live estate round-trips through git and a fresh box converges from one file:
`export | apply -f -` is a no-op. Both lists may live in one document
(`checks:` and `netgroups:`); each export emits only its own and each apply
reads only its own. Output is ordered by name, so a re-export diffs cleanly.

A check row carries `name`, `kind`, `interval` (a Go duration), `enabled`,
and its `targets` with each kind's own fields — tcp/ping
`address`+`port`, http `url`+`expect`, tls `address`+`port`+`min_days`, dns
`dns_name`+`expect_addr`, metric `url`+`op`+`value`. A netgroup row carries
`name`, `includes`, and `excludes`. An omitted `enabled` starts the check
enabled. Validation is the store's own `ParseCheckTargets`/`ParsePrefixList`
and create/update rules, so a file that would not survive `check add` is
refused the same way.

`apply` reconciles by name: a missing row is created, a present one is updated
in place, an identical one is reported unchanged. A check's **kind is not
updatable** — a file that changes a stored check's kind is refused by name,
naming the check, rather than silently recreated (which would discard its
`check_state`); remove and re-add it deliberately. `--prune` deletes every row
absent from the file and needs confirmation: pass `--yes-really` (or the
global `--assume-yes`). Without it the prune is refused non-interactively, the
same gate the global silence uses.
