---
type: Guide
title: "Reactive automation"
description: "Webhook ingress, CEL event rule routing, push alerts, silence windows, and approval gates."
status: stable
tags: [guide, reactive, cel, webhooks, alerts, approvals]
---

# Reactive automation

exocomp can react automatically to monitoring alerts, webhooks, and internal health checks through an embedded Common Expression Language (CEL) evaluation engine.

Every triggered run is submitted to the single job queue, ensuring all reactive executions adhere to the same sandboxing, redacting, and logging constraints.

## Webhook ingress

Each source has a hook URL, printed by `exocomp serve` at startup:

```
hook alertmanager  http://127.0.0.1:8642/hook/<token>
hook healthchecks  http://127.0.0.1:8642/hook/<token>
hook generic       http://127.0.0.1:8642/hook/<token>
```

The token in the path is the credential — there is no bearer header — and it is derived
from the envelope, so it survives a restore and is never written down. Point a monitor
at its source's URL; Alertmanager posts its own v4 webhook body there. For a sender with
no adapter, post any JSON object to the `generic` hook: the body must carry a `name`,
`alertname`, `title`, or `event` key, and every top-level scalar becomes a label a rule
can read:

```bash
curl -X POST http://127.0.0.1:8642/hook/<generic-token> \
  -H "Content-Type: application/json" \
  -d '{"name": "DiskPressure", "host": "nas.lan", "severity": "warning"}'
```

## Writing CEL event rules

Define event rules using `exocomp rule add`:

```bash
exocomp rule add auto-disk-cleanup \
  --source alertmanager \
  --expr "event.name == 'DiskPressure' && event.labels['severity'] == 'warning'" \
  --action playbook \
  --repo-source homelab \
  --playbook playbooks/clean-cache.yml \
  --limit nas.lan \
  --require-approval
```

A playbook rule declares no run-secrets binding or vault project of its own. Each fire inherits
the source's default run-secrets binding when one is set (`exocomp source run-secrets`), which is
how a rule decrypts a tree whose vault password is an env var.

The `--expr` expression is compiled and evaluated using Google CEL:
- Safe execution: CEL expressions cannot invoke system calls, allocate unbounded memory, or hang indefinitely.
- Idempotent: Rules evaluate strictly against the normalized event payload and system status.

An expression reads the normalized `event`, which carries `source`, `kind`, `name`,
`status`, `severity`, `host`, `instance`, `fingerprint`, `summary`, and the `labels` and
`annotations` maps. There is no `payload` field: a sender's own body is normalized onto
those fields, with Alertmanager's `alertname` and `severity` labels landing on
`event.name` and `event.severity`.

## Approval gates and notifications

If a rule sets `--require-approval`, the triggered job does not run immediately. Instead:
1. An approval request is recorded in the SQLite step ledger.
2. A push notification is dispatched to your configured `ntfy` topic (configurable in the Web UI via the contextual setup modal on `/system`):

   <p><img src="../design/assets/system/1c-ntfy-states.png" alt="Push notifications drawer"></p>

3. The operator approves or rejects the execution from the Web UI, TUI, or mobile interface.

## Silence windows

To suppress automated remediations during planned maintenance:

```bash
exocomp silence add \
  --host nas.lan \
  --reason "Planned storage upgrade" \
  --for 2h
```

Or silence by alert name:

```bash
exocomp silence add \
  --alert DiskPressure \
  --reason "Investigating disk space" \
  --for 1h
```

Any matching events that arrive during the silence window are recorded in the event log but will not trigger associated actions.


## Host self-service requests

A managed host can request a pre-approved job that runs on itself without holding
an operator token:

```bash
curl -X POST https://exocomp.example/api/v1/hosts/self/requests   -H "Authorization: Bearer $HOST_TOKEN"   -H "Idempotency-Key: pre-reboot-$(hostname)"   -H "Content-Type: application/json"   -d '{"request":"upgrade","reason":"before reboot"}'
```

The host's canonical identity is taken from the token, never from the body. A
request either fires immediately, lands in `awaiting_approval`, or is suppressed
by the same cooldown/cap/dedup guardrails as any other event; `?wait=true` (with
an optional `timeout=30s`) blocks until the job reaches a terminal state.
