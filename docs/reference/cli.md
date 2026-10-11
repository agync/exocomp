---
type: Reference
title: "Command line interface (CLI)"
description: "Global flags, verbs, remote driving, and machine-readable output envelopes."
status: stable
tags: [reference, cli, commands, flags]
---

# Command line interface (CLI)

The `exocomp` binary functions as both the server daemon (`exocompd` / `exocomp serve`) and the primary operator CLI client.

## Global flags

These flags apply to all verbs:

| Flag | Env Variable | Description |
|---|---|---|
| `--data-dir <path>` | `EXOCOMP_DATA_DIR` | Path to data directory (default `$XDG_DATA_HOME/exocomp` or `/var/lib/exocomp`) |
| `--output <format>` | `EXOCOMP_OUTPUT` | Output formatting: `text` (default) or `json` |
| `--remote <url>` | `EXOCOMP_REMOTE` | Drive a remote daemon over HTTPS / LAN |
| `--token <token>` | `EXOCOMP_TOKEN` | Bearer token for remote daemon authentication |

## Primary verbs

### Automation & Execution
- `exocomp playbook run <path>`: Run an Ansible playbook against a source.
- `exocomp command run <cmd>`: Run an ad-hoc shell command on target nodes.
- `exocomp workspace plan <name>`: Generate and seal an OpenTofu execution plan.
- `exocomp workspace apply <name>`: Apply a sealed OpenTofu plan.
- `exocomp handoff run <name>`: Execute a chained OpenTofu apply → Ansible converge workflow.
- `exocomp jobs list`: View active and historic queue jobs.
- `exocomp jobs tail <id>`: Stream live stdout/stderr for a running job.
- `exocomp jobs redrive <id>`: Idempotently re-run a failed job using the original parameters.

### State & Secrets
- `exocomp source add/list/sync/rm`: Manage registered Git and local sources.
- `exocomp secret set/list/rotate/rm`: Manage age secrets envelope entries.
- `exocomp host list/key/add/rm`: Inspect SSH fleet keys and trusted host fingerprints.

### Cluster & Maintenance
- `exocomp serve`: Run the control plane daemon.
- `exocomp doctor`: Run environment, permissions, and toolchain checks.
- `exocomp drain`: Cordon the daemon and await completion of in-flight work before host reboots.
- `exocomp reload`: Reload schedules, transports, and CEL rules from disk.
- `exocomp upgrade`: Run zero-sudo staged self-upgrade with automated rollback.
- `exocomp backup run`: Trigger an immediate snapshot across all configured backup legs.
- `exocomp restore`: Rebuild data directory from a Restic, S3, or Git snapshot.
- `exocomp kit print`: Output the physical disaster recovery card.

## Machine-readable JSON output

Passing `--output json` emits a structured envelope with deterministic error codes:

```json
{
  "kind": "job",
  "status": "success",
  "data": {
    "id": "job_01j7abc...",
    "kind": "ansible.playbook",
    "exit_code": 0,
    "duration_ms": 1420
  }
}
```
