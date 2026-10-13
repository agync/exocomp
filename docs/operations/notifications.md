---
type: Guide
title: "Setup, notifications and metrics"
description: "The first-run wizard, the ntfy push transport, the muted versus paused holds, and the metrics staleness gauges."
status: stable
tags: [operations, notifications, metrics]
---

# Setup, notifications, and metrics

First-run identity, the two ways to hold notifications, and what the daemon exposes about
its own health.

## First-run setup

`exocomp setup` is a four-step wizard: identity → recovery kit → first backup and drill →
hand over. Nothing is written to disk until the final step, so quitting earlier leaves no
trace. The recovery-kit step blocks until you confirm you have stored the key.

Step four is the single write. It mints the identity and keyring, then persists the
operator display name and the generated ntfy topic to the store, and attempts a test push.
The topic is ordinary configuration — never a sealed secret — and the ntfy server URL and
access token are configured elsewhere. When only a topic has been minted and no push
transport is configured yet, the hand-over screen says so plainly rather than claiming a
push went out — configure a server URL and token to enable it (below).

## The push transport

Approvals and alerts reach a phone over ntfy. The server URL and topic are ordinary
configuration edited from the system page's push channel modal; only the optional access
token is a sealed secret. An operator who has not set one up gets a working daemon and
a silent phone, not a daemon that refuses to start:

```
exocomp secrets add ntfy/token      # mandatory on a public server
```

configure the URL and topic per channel on the system page's ntfy row.

A bare `ntfy.sh` topic is unauthenticated — readable and publishable by anyone who guesses
it — so the transport refuses a public topic without an access token, because approval
links are bearer capabilities.

### GoAlert

A second transport pages an on-call rotation through a GoAlert Generic API integration
key. It is loaded the same way — from sealed secrets, nil when unconfigured — and both
transports fan out together: seal one, the other, or both.

```
exocomp secrets add goalert/url         # e.g. https://goalert.lan
exocomp secrets add goalert/key         # the Generic API integration key
exocomp secrets add goalert/namespace   # optional dedup prefix for multiple instances
```

A firing notification opens an incident; a resolved one closes the matching incident,
keyed by a dedup derived from the alert's source and host so the two halves line up. The
integration key is a credential: it lives only in the envelope and never appears in a log
or an error string, even when the server is unreachable.

## Job-failure paging

Beyond approvals, the daemon pages once for every job that ends in a terminal failure — a
scheduled converge, a backup, a drift apply — that would otherwise run completely silent.
The page is high priority, attributed by runbook and workspace so a scoped mute can hold
it, and carries the run's last error. It is idempotent: a job already paged on never pages
again, across repeated sweeps or a restart.

## Holding notifications: MUTED vs PAUSED

Two different holds, shown as distinct banner states on the dashboard and the TUI:

| State | Source | Effect |
|---|---|---|
| **MUTED** | a `notify_mute` | The push is held; **jobs still run** and outcomes are recorded |
| **PAUSED** | the global reactive kill switch (a global silence) | Reactive jobs are held and matching alerts are swallowed |

A mute is always time-bounded — there is no indefinite mute, so a phone cannot go quiet
the week the backups start failing. A scoped silence swallows a matching alert before it
becomes a job; the global kill pauses every rule. A scoped (non-global) silence shows a
**SILENCED** banner — distinct from **PAUSED** and **MUTED** — carrying the count of alerts
swallowed while it is active, and it lifts automatically at its expiry.

## Metrics

`exocomp serve` exposes `GET /metrics` on the same loopback backend, with staleness gauges
and the seeded alert rules. Point a scraper at it to alert on the control plane's own
freshness — the last successful backup, the oldest unanswered approval, and the like.
`exocomp doctor` prints the same self-observability lines for a one-shot check.

The loopback backend is reachable by tofu-tier jobs, so the scrape is **bearer-gated**:
every request must carry the `metrics-scrape` bearer, a token
derived from the envelope like the state backend's own (ADR 0004). Configure Prometheus
with it as an `authorization` credential; an unauthenticated scrape is refused with `401`.

Related: [Reacting to alerts](reactive.md) · [LAN access](lan-access.md) ·
[Disaster recovery](disaster-recovery.md).
