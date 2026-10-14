---
type: Guide
title: "Sources and inventory"
description: "Registering Git repositories, local directories, managing SSH fleet identities, and auto-onboarding hosts."
status: stable
tags: [guide, sources, git, inventory, ssh]
---

# Sources and inventory

All automation in exocomp executes against registered **Sources**. A source represents a Git repository, local directory, or storage mount containing Ansible playbooks, roles, collections, and OpenTofu configurations.

## Registering a source

Register a Git repository with `exocomp source add`:

```bash
exocomp source add homelab \
  --url git@github.com:example/homelab.git \
  --ref main
```

Or register a local path source:

```bash
exocomp source add local-homelab --path /srv/homelab
```

View configured sources in the CLI or Web UI:

```bash
exocomp source list
```

```
NAME     KIND  REF    LAST SYNC            TRUST
homelab  git   main   2026-10-11 22:30:00  trusted
```

In the Web UI, the sources list shows synchronization health, branch tracking, and active execution cards. Git sources can be synchronized from the CLI with `exocomp source sync <name>` or via the **Sync now** button in the Web UI:

![Sources list view](../design/assets/web/1c-sources.png)

Selecting a source opens its detail view, where you can inspect playbooks, run histories, trigger syncs, and launch new executions:

![Source detail drawer](../design/assets/web/1d-source-detail.png)

## Dependency management in sources

exocomp enforces strict per-source isolation:

1. **Python dependencies (`pyproject.toml` or `requirements.txt`)**:
   If a playbook directory contains a Python dependency manifest, exocomp uses `uv` to build an isolated virtualenv under `cache/python/<source>/<hash>` (Decision 0011). If no manifest is present, it executes using the pinned container toolchain.
2. **Ansible collections (`requirements.yml`)**:
   Per-source collection manifests are resolved via `ansible-galaxy` into `cache/collections/<source>/<hash>` (Decision 0015). Under the enforcing sandbox, local collection artifact paths (e.g. `type: dir` or `type: file` pointing to sibling directories) are granted read-only permissions automatically so collections install without sandbox permission violations.

## SSH fleet keys and host onboarding

To manage target nodes without distributing individual SSH keys, exocomp maintains a single **fleet identity key** in the age envelope.

1. **Onboard target hosts**:
   ```bash
   exocomp host onboard router.lan server0.lan
   ```
   `host onboard` keyscans each host, records its host key in the backed-up `known_hosts` store, ensures the sealed fleet SSH key exists, and prints the fleet public key to add to the target node's `~/.ssh/authorized_keys`.

2. **Inspect recorded host keys**:
   ```bash
   exocomp host known-hosts
   ```
   exocomp strictly verifies host fingerprints against its trusted `known_hosts` store. Connecting to an unpinned or changed host key is refused.
