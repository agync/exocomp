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
     --type tcp \
     --target 192.168.1.1:53 \
     --netgroup lan \
     --interval 30s
   ```

## The checks matrix in the Web UI

In the Web UI, the Checks view renders target status in an interactive card grid:

![Reachability checks interface](../design/assets/web/1b-checks.png)

The UI adheres strictly to the design invariant: **State is colour plus word**. A failing target renders as a red badge containing the explicit text `DOWN`, while an unpolled target displays `UNOBSERVED`.

## Check-triggered runs

When a check target transitions between states (e.g. from `UP` to `DOWN`), exocomp emits an internal event. You can bind reactive rules to trigger an automated playbook run when a service fails:

```yaml
# .exocomp/rules/dns-remediation.yml
name: restart-dns-on-failure
on: check.status_change
when: "event.check == 'dns-resolver' && event.status == 'DOWN'"
action: playbook
playbook: playbooks/restart-dns.yml
source: homelab
require_approval: false
```
