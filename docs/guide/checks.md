---
type: Guide
title: "Health checks and probes"
description: "Network groups, reachability probe targets, status matrix, and check-triggered runs."
status: stable
tags: [guide, checks, probes, netgroups, monitoring]
---

# Health checks and probes

exocomp includes a native reachability monitoring system that tracks the health of internal services and network targets, enabling automated remediation without external polling daemons.

## Network groups and probe targets

1. **Network groups (`netgroup`)**:
   Define named CIDR blocks to classify hosts and services by topological location:
   ```bash
   exocomp netgroup add lan 192.168.1.0/24
   exocomp netgroup add tailnet 100.64.0.0/10
   ```

2. **Probes and checks**:
   Register reachability checks against targets:
   ```bash
   exocomp check add dns-resolver \
     --kind tcp \
     --target resolver=192.168.1.1:53 \
     --interval 30s
   ```
   Targets are classified automatically into matching `netgroup` definitions based on their IP address.

## The checks matrix in the Web UI

In the Web UI, the Checks view renders target status in an interactive card grid:

<p><img src="../design/assets/web/1b-checks.png" alt="Reachability checks interface"></p>

The UI adheres strictly to the design invariant: **State is colour plus word**. A failing target renders as a red badge containing the explicit text `DOWN`, while an unpolled target displays `UNOBSERVED`.

## Check-triggered runs

When a check target transitions between states, exocomp emits an internal event. You
can bind reactive rules to trigger an automated playbook run when a service fails:

```bash
exocomp rule add restart-dns-on-failure \
  --source check \
  --expr "event.name == 'dns-resolver' && event.host == 'resolver' && event.status == 'resolved'" \
  --action playbook \
  --repo-source homelab \
  --playbook playbooks/restart-dns.yml \
  --limit dns-servers
```

A check event names the check in `event.name` and the probed target in `event.host`. The
status is presence-oriented: `firing` when a target comes **up**, `resolved` when it goes
**down** — so a "restart on failure" rule matches `resolved`. See
[Reacting to alerts](../operations/reactive.md#checks-as-an-event) for the full event
shape and the hysteresis that precedes a transition.
