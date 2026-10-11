---
type: Guide
title: "Quickstart"
description: "From zero to a running homelab control plane: Quadlet deploy, init wizard, recovery card, and doctor check."
status: stable
tags: [guide, quickstart, setup, bootstrap]
---

# Quickstart

exocomp runs as a single rootless container (`exocompd`) holding one Go binary, a SQLite database, and the pinned execution toolchain (`ansible-core`, `opentofu`, `restic`, `sops`, `age`, `git`).

Two hard constraints govern setup: **trivial bootstrap** and **trivial disaster recovery**. A setup is only considered complete once the recovery card is printed and verified.

## 1. Deploy the Quadlet container

Install the Quadlet definition into your user systemd directory:

```bash
mkdir -p ~/.config/containers/systemd
cp packaging/quadlet/exocomp.container ~/.config/containers/systemd/
cp packaging/quadlet/exocomp.volume ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start exocomp
```

The container starts rootless, mounts its dedicated volume to `/var/lib/exocomp`, and listens on loopback `127.0.0.1:8642`.

## 2. Complete the initialization wizard

Run the interactive setup wizard via CLI or open the browser at `http://127.0.0.1:8642`:

```bash
exocomp init
```

The wizard guides through six idempotent steps:
1. **Host identity**: Generates the age fleet identity key and wraps it with `systemd-creds`.
2. **Secrets envelope**: Generates the root Data Encryption Key (DEK) and derives HKDF subkeys.
3. **Backup legs**: Configures the three independent disaster recovery targets (local NAS, S3, Git).
4. **Push notifications**: Configures the ntfy push topic for alert routing and approvals.
5. **Git source**: Connects your homelab configuration Git repository.
6. **Recovery kit verification**: Verifies that off-host credentials allow opening test snapshots.

## 3. Print the recovery card

Disaster recovery never depends on the running system. Generate the physical recovery card:

```bash
exocomp kit print > recovery-card.txt
```

Print this card or save it onto physical media. The card contains:
- The age recovery secret key.
- The off-host read-only S3 and Git credentials.
- The exact shell commands to restore the control plane onto a blank machine using only `age`, `restic`, and `curl` (no exocomp binary required).

## 4. Verify system health

Run `exocomp doctor` to verify environment permissions, sandboxing, and dependencies:

```bash
exocomp doctor
```

```
go runtime:   go1.23.0
data dir:     /var/lib/exocomp — ok (mode 0700)
secrets:      mlock active, host key encrypted via systemd-creds
executor:     nono (tier: enforcing)
optional tools (probe subset):
  ansible     2.17.3
  opentofu    1.8.1
  restic      0.17.0
  uv          0.4.0
status:       HEALTHY
```

## 5. Explore the dashboard

Open `http://127.0.0.1:8642` to view the attention-first dashboard:

![exocomp web dashboard](../design/assets/web/1a-shell-dashboard.png)

The dashboard presents the system status before detail: active alerts, pending approval gates, backup freshness, and running queue jobs.
