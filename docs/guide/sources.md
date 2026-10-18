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

<p><img src="../design/assets/web/1c-sources.png" alt="Sources list view"></p>

Selecting a source opens its detail view, where you can inspect playbooks, run histories, trigger syncs, and launch new executions:

<p><img src="../design/assets/web/1d-source-detail.png" alt="Source detail drawer"></p>

## Source-scoped egress default

A source whose playbooks always need the same extra port records it once, beside the other
source verbs:

```bash
exocomp source egress homelab 443 9090
```

`source egress <name> [port...]` sets the source's default egress set, replacing any previous
one; with no ports it clears the declaration. Every ansible run that names the source inherits
the set at queue admission, unioned with the run's own `--egress` grant and the ansible kind's
built-in SSH port 22. It is **operator-set through the daemon only** — a checked-out playbook
can never grant itself a port. The source's detail page shows the default (an **Egress
default** card), and the rules page shows each playbook rule's effective egress. See
[Source-scoped egress default](../operations/sources.md#source-scoped-egress-default).

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

## Using a different key for one source

exocomp's fleet key is deliberately **one key for every host**. If one source, inventory,
or subset of hosts needs its own identity, bring the key yourself and wire it in as an
ordinary Ansible credential, with the sandbox grant that lets the run read it:

1. Store the private key outside the source checkout; a committed or checked-out
   controller key widens the repo's trust boundary and never needs to be there.
2. Reference it in the source's `group_vars` or inventory:
   `ansible_ssh_private_key_file: /run/keys/sources/infra-deploy`
3. Grant that file to the run:
   ```bash
   exocomp playbook run site.yml --source infra --ssh-identity /run/keys/sources/infra-deploy
   ```
4. Push its public half to the target nodes with your own playbook — the onboarding
   runbook authorizes only the fleet public key.

The key declaration is per job and defaults to nothing; `--ssh-identity` is documented
fully in [playbook run flags](../operations/playbook-run.md) and
[running the daemon](../operations/serve.md). A run cannot read `~/.ssh` or any
directory, only the exact named file(s).

Note that in v1 the recorded `known_hosts` is **not yet enforced** by a run and the
fleet (or any other) key is **not yet loaded into a per-job ssh-agent** (v1.1). Until
then Ansible's own connection settings apply. See
[Managed-host SSH in operations/sources.md](../operations/sources.md).
