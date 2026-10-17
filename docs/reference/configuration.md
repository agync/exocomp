---
type: Reference
title: "Configuration reference"
description: "Specification and schemas for .exocomp/hooks.yml, Quadlet container units, and environment variables."
status: stable
tags: [reference, configuration, hooks, quadlet, yaml]
---

# Configuration reference

## Source hooks (`.exocomp/hooks.yml`)

Placed at the root of a registered Git or directory source. Defines automated pre-run/post-run commands and health check probe gates.

```yaml
version: "1"

# Ordered pre-run steps executed before playbooks/applies
pre_run:
  - name: verify-ansible-lint
    command: ansible-lint playbooks/site.yml
    continue_on_error: false

# Ordered post-run steps executed after successful runs
post_run:
  - name: notify-sync
    command: curl -s -X POST https://status.lan/ping

# Declared health probes gating execution
health_gates:
  - probe: dns-resolver
    expected: UP
    timeout: 10s
```

## Systemd Quadlet unit (`exocomp.container`)

Controls rootless container execution under user systemd:

```ini
[Unit]
Description=exocomp control plane
After=exocomp.volume

[Container]
Image=ghcr.io/agync/exocomp:latest
ContainerName=exocomp
Volume=exocomp.volume:/var/lib/exocomp
UserNS=keep-id
Environment=XDG_DATA_HOME=/var/lib
Environment=EXOCOMP_IMAGE=ghcr.io/agync/exocomp:latest
LoadCredentialEncrypted=host.key:/var/lib/exocomp/host.key.cred
Exec=serve

[Service]
Restart=always
RestartSec=5
MemoryMax=1.5G

[Install]
WantedBy=default.target
```

## Environment variables

| Variable | Description |
|---|---|
| `EXOCOMP_DATA_DIR` | Directory holding `exocomp.db`, `keyring.age`, and caches |
| `EXOCOMP_OUTPUT` | Output formatting: `text` or `json` (or `yaml` for export) |
| `EXOCOMP_REMOTE` | HTTPS endpoint or pinned host of a remote daemon when using the CLI as a client |
| `EXOCOMP_TOKEN` | Bearer token for client authentication |
| `EXOCOMP_IDEMPOTENCY_KEY` | Client-provided deduplication key for mutating requests |
| `EXOCOMP_SANDBOX` | Job isolation tier default: `enforcing`, `opportunistic`, or `disabled` |
| `EXOCOMP_IMAGE` | Container image ref recorded by the daemon for disaster recovery |
| `EXOCOMP_PUBLIC_URL` | Public base URL for decision links in notifications |
| `NO_FLYLINE` | Disable animated terminal flyline progress during CLI runs |
