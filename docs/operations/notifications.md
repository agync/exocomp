# Setup, notifications, and metrics

First-run identity, the two ways to hold notifications, and what the daemon exposes about
its own health.

## First-run setup

`exocomp setup` is a four-step wizard: identity → recovery kit → first backup and drill →
hand over. Nothing is written to disk until the final step, so quitting earlier leaves no
trace. The recovery-kit step blocks until you confirm you have stored the key.

Step four is the single write. It mints the identity and keyring, then persists the
operator display name and the generated ntfy topic to the store, seals the topic as the
notify config, and attempts a test push. When only a topic has been minted and no push
transport is configured yet, the hand-over screen says so plainly rather than claiming a
push went out — seal an ntfy server URL to enable it (below).

## The push transport

Approvals and alerts reach a phone over ntfy. The transport is configured from sealed
secrets, so an operator who has not set one up gets a working daemon and a silent phone,
not a daemon that refuses to start:

```
exocomp secrets add ntfy/url        # e.g. https://push.lan
exocomp secrets add ntfy/topic      # the wizard seals this one for you
exocomp secrets add ntfy/token      # mandatory on a public server
```

A bare `ntfy.sh` topic is unauthenticated — readable and publishable by anyone who guesses
it — so the transport refuses a public topic without an access token, because approval
links are bearer capabilities.

## Holding notifications: MUTED vs PAUSED

Two different holds, shown as distinct banner states on the dashboard and the TUI:

| State | Source | Effect |
|---|---|---|
| **MUTED** | a `notify_mute` | The push is held; **jobs still run** and outcomes are recorded |
| **PAUSED** | the global reactive kill switch (a global silence) | Reactive jobs are held and matching alerts are swallowed |

A mute is always time-bounded — there is no indefinite mute, so a phone cannot go quiet
the week the backups start failing. A scoped silence swallows a matching alert before it
becomes a job; the global kill pauses every rule.

## Metrics

`exocomp serve` exposes `GET /metrics` on the same loopback backend, with staleness gauges
and the seeded alert rules. Point a scraper at it to alert on the control plane's own
freshness — the last successful backup, the oldest unanswered approval, and the like.
`exocomp doctor` prints the same self-observability lines for a one-shot check.

Related: [Reacting to alerts](reactive.md) · [LAN access](lan-access.md) ·
[Disaster recovery](disaster-recovery.md).

A scoped (non-global) silence shows a **SILENCED** banner (distinct from the
global-pause **PAUSED** and the notification **MUTED**), carrying the count of
alerts swallowed while it is active; it lifts automatically at its expiry.
