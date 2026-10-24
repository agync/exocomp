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

These flags apply across all CLI commands:

| Flag | Env Variable | Description |
|---|---|---|
| `--output <format>` | `EXOCOMP_OUTPUT` | Output formatting: `text` (default) or `json` (also `yaml` for export) |
| `--remote <target>` | `EXOCOMP_REMOTE` | Drive a remote daemon over HTTPS / LAN (pinned name or URL); commands without a remote path refuse it |
| `--token <token>` | `EXOCOMP_TOKEN` | Bearer token for remote daemon authentication |
| `--idempotency-key <key>` | `EXOCOMP_IDEMPOTENCY_KEY` | Deduplicate mutating commands to one job |
| `-y, --assume-yes` | — | Answer yes to destructive confirmations (scripts, CI) |
| `--no-input` | — | Refuse confirmations instead of waiting on stdin |

*(Note: Data directory defaults to `$XDG_DATA_HOME/exocomp` or `/var/lib/exocomp`, and can be customized via `EXOCOMP_DATA_DIR` or `--data-dir` on lifecycle commands).*

## Primary verbs

### Automation & Execution
- `exocomp playbook run <path>`: Run an Ansible playbook against a source.
- `exocomp command run <cmd>`: Run an ad-hoc shell command on target nodes.
- `exocomp workspace plan <name>`: Generate and seal an OpenTofu execution plan.
- `exocomp workspace apply <name>`: Apply an OpenTofu workspace or sealed plan.
- `exocomp provision run <workspace> <playbook>`: Execute chained apply → await SSH → onboard → configure workflow.
- `exocomp handoff run <workspace>`: Execute the Terraform → Ansible inventory handoff.
- `exocomp jobs list`: View active and historic queue jobs.
- `exocomp jobs follow <id>`: Stream live events for a running job until terminal.
- `exocomp jobs events <id>`: Show a job's append-only event log.
- `exocomp jobs redrive`: Idempotently re-run jobs cut off by restart or failure.

### State & Secrets
- `exocomp source add/list/sync/rm`: Manage registered Git and local sources.
- `exocomp source run-secrets <name> [binding]`: Set the default run-secret binding a source's ansible runs inherit; an explicit `--run-secrets` wins.
- `exocomp secrets add/list/replace/rm`: Manage age secrets envelope entries.
- `exocomp run-secret add/list/rm`: Bind sealed secrets to named environment sets for runs.
- `exocomp host onboard/known-hosts`: Onboard fleet nodes and inspect trusted host keys.

### Checks & Reactive
- `exocomp check add/list/enable/disable/rm`: Manage network reachability probe checks.
- `exocomp netgroup add/list/rm`: Manage named CIDR network groups.
- `exocomp rule add/list/enable/disable/rm`: Manage CEL event-to-job rules.
- `exocomp silence add/list/rm`: Temporarily swallow matching reactive events.

### Cluster & Maintenance
- `exocomp setup`: First-run interactive wizard (identity, recovery kit, backup drill, hand over).
- `exocomp serve`: Run the control plane daemon.
- `exocomp doctor`: Run environment, permissions, and toolchain checks.
- `exocomp drain`: Cordon the daemon and await completion of in-flight work before host reboots.
- `exocomp reload`: Reload schedules and push transports from the store without stopping.
- `exocomp restart`: Drain in-flight jobs and re-exec the binary in place.
- `exocomp upgrade`: Run zero-sudo staged self-upgrade with automated rollback.
- `exocomp backup`: Assemble a snapshot and fan it out to configured targets.
- `exocomp restore`: Rebuild data directory from a Restic, S3, or Git snapshot.
- `exocomp recovery-kit`: Re-render recovery kit structure from stored public data.
- `exocomp kit derive/verify`: Derive secrets from the recovery card and verify printed pages.

## Machine-readable JSON output

Passing `--output json` emits a structured RFC 9457-compatible envelope with schema version:

```json
{
  "schema_version": 1,
  "kind": "job",
  "data": {
    "id": "job_01j7abc...",
    "kind": "ansible.playbook",
    "exit_code": 0,
    "duration_ms": 1420
  }
}
```
