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

### Control plane egress

Under the default `enforcing` sandbox tier, an ansible run reaches TCP 22 — SSH to inventory hosts — and nothing else. A control plane playbook that talks to an API over HTTPS (NextDNS, a Tailscale tailnet, a UniFi controller) needs port 443, which the run declares with `--egress`:

```bash
exocomp playbook run playbooks/tailscale.yml \
  --source homelab \
  --diff \
  --egress 443
```

`--egress` names one or more ports — repeatable, or comma-separated — and grants them on top of the built-in SSH port 22. `run --egress` does the same for a handoff, and a playbook rule carries it for every run it triggers (`rule add --action playbook --egress 443`). The declared set is recorded on the job and shown in the run's configuration.

The grant is a port, not a destination: Landlock cannot scope it to one host, so `--egress 443` opens 443 to any host the job can reach. Declare only the ports a run genuinely needs, and leave host-level restriction to the network.

A run's effective set is the union of three grants: the ansible kind's built-in SSH port 22, any **operator-set source default**, and whatever that one run declares. The source default is the place for a port every run from that source needs (a controller API over 443, a metrics endpoint), so it is declared once on the source instead of repeated on each run:

```bash
exocomp source egress homelab 443 9090
```

`source egress <name> [port...]` records the default, replacing any previous one; with no ports it clears the declaration. It is **operator-set through the daemon only** — a repo can never declare its own widening, so a checked-out playbook cannot grant itself a port. The default is shown on the source's detail page (the Egress ports line and a form for the same verb), and every ansible run that names the source inherits it at queue admission, where the union is recorded on the job row.

A playbook that needs more than a port is the one case for `--privileged`, a per-run opt-out of the kernel sandbox: operator-only, off by default, and audited, so reserve it for work that must run unsandboxed on the controller itself. The product shows it wherever a run is read — the job detail's run card wears an `unsandboxed` chip, the jobs list names it beside the source, the run's log opens with an attributed "run requested UNSANDBOXED" band, and `exocomp doctor` reports how many recent runs ran unsandboxed. When only a remote host needs the elevation (or the egress), prefer reaching that host over SSH so the work happens on the far side and the daemon's sandbox stays on.

## Live job streaming and execution logs

When a job runs, the Web UI streams logs and execution progress in real time:

<p><img src="../design/assets/jobs/1c-job-log-tail.png" alt="Live job log tail"></p>

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
    --cron "0 0 4 * * *" \
    --kind play \
    --check \
    --target tf:homelab \
    --source homelab \
    -- playbooks/site.yml
  ```
  `--source` runs the play from that source's checkout, so the playbook path is relative to it.
- In the Web UI, schedules can also be triggered on demand using the **Run now** action on `/schedules`.

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

<p><img src="../design/assets/jobs/1b-job-detail-degraded.png" alt="Job detail in degraded state"></p>
