---
type: Guide
title: "Ansible automation"
description: "Executing playbooks, ad-hoc commands, config-drift detection, and handling unreachable nodes."
status: stable
tags: [guide, ansible, playbooks, drift, automation]
---

# Ansible automation

exocomp runs Ansible playbooks and ad-hoc commands through its one audited job queue, sandboxed under the `nono` driver. Exit codes alone decide execution success; stdout/stderr streams enrich the UI.

## Executing playbooks

Trigger a playbook execution via the CLI:

```bash
exocomp playbook run playbooks/site.yml \
  --source homelab \
  --diff
```

Common flags:
- `--limit <pattern>`: Target specific hosts or groups.
- `--tags <tag1,tag2>` / `--skip-tags <tag3>`: Filter tasks.
- `-e <key=value>`: Pass extra variables. Arguments are passed securely off `argv` to avoid leaking variable values in process tables.
- `--require-approval`: Require an explicit operator decision before execution starts.

### Control plane egress (`--privileged`)

Under the default `enforcing` sandbox tier, jobs have no network egress beyond local LAN and configured endpoints.

For control plane playbooks that manage cloud or SaaS APIs (such as NextDNS, Tailscale Tailnet, or UniFi controllers), pass the `--privileged` flag to permit external egress:

```bash
exocomp playbook run playbooks/tailscale.yml \
  --source homelab \
  --diff \
  --privileged
```

## Live job streaming and execution logs

When a job runs, the Web UI streams logs and execution progress in real time:

![Live job log tail](../design/assets/jobs/1c-job-log-tail.png)

Jobs display structured timing, execution parameters, and output logs. Completed jobs retain their full output logs in SQLite storage for post-mortem analysis.

## Config drift detection

Running a playbook in check mode acts as a drift-detection probe:

```bash
exocomp playbook run playbooks/site.yml \
  --source homelab \
  --check
```

- If tasks report `changed`, exocomp marks the job outcome as `drift` and emits an internal drift event.
- You can schedule periodic drift scans:
  ```bash
  exocomp schedule add daily-drift \
    --cron "0 4 * * *" \
    --kind play \
    --check \
    --source homelab \
    --playbook playbooks/site.yml
  ```

## Ad-hoc commands and unreachable hosts

Run one-off commands across nodes under the same sandboxing and governance as full playbooks:

```bash
exocomp command run "uptime" --limit servers --source homelab
```

For roaming laptops or homelab nodes that may be temporarily powered off, pass `--tolerate-unreachable`:

```bash
exocomp playbook run playbooks/workstations.yml \
  --source homelab \
  --tolerate-unreachable
```

If hosts are unreachable, the job completes with a `degraded` state instead of raising a critical failure notification:

![Job detail in degraded state](../design/assets/jobs/1b-job-detail-degraded.png)
