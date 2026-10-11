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

Send external events (e.g. from Alertmanager, Grafana, or GitHub) to the ingress endpoint:

```bash
curl -X POST http://127.0.0.1:8642/api/v1/events \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "source": "alertmanager",
    "kind": "alert",
    "payload": {
      "alertname": "DiskPressure",
      "instance": "nas.lan",
      "severity": "warning"
    }
  }'
```

## Writing CEL event rules

Define event rules in `.exocomp/rules/` inside your Git source:

```yaml
# .exocomp/rules/disk-cleanup.yml
name: auto-disk-cleanup
on: alertmanager
when: "event.payload.alertname == 'DiskPressure' && event.payload.severity == 'warning'"
action: playbook
playbook: playbooks/clean-cache.yml
source: homelab
limit: "{{ event.payload.instance }}"
require_approval: true
```

The `when` expression is compiled and evaluated using Google CEL:
- Safe execution: CEL expressions cannot invoke system calls, allocate unbounded memory, or hang indefinitely.
- Idempotent: Rules evaluate strictly against the event payload and system status.

## Approval gates and notifications

If a rule sets `require_approval: true`, the triggered job does not run immediately. Instead:
1. An approval request is recorded in the SQLite step ledger.
2. A push notification is dispatched to your configured `ntfy` topic:

   ![Push notifications drawer](../design/assets/system/1c-ntfy-states.png)

3. The operator approves or rejects the execution from the Web UI, TUI, or mobile interface.

## Silence windows

To suppress automated remediations during planned maintenance:

```bash
exocomp silence add "Planned storage upgrade" \
  --match "event.payload.instance == 'nas.lan'" \
  --duration 2h
```

Any matching events that arrive during the silence window are recorded in the event log but will not trigger associated actions.
