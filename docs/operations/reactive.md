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
windows; `--sweep-interval` sets how often TTLs are enforced and drift collected.

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
needs `--yes-really`: it swallows every event from every source. It does **not**
stop running jobs finishing and it does **not** stop schedules firing — it stops
events becoming new jobs. `silence list` shows what is active and how long is
left; `silence rm <id>` lifts one early.

## Answering

```sh
exocomp approval list
exocomp approval link 3      # prints the two decision URLs
```

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
