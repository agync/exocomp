# Operations and cluster lifecycle

Day-2 production operations, node-zero deployment, disaster recovery drills, and cluster maintenance.

For everyday task workflows (running playbooks, OpenTofu, checks), see the [User guide](../guide/index.md).

## Cluster lifecycle and maintenance

| Runbook | Description |
|---|---|
| [Bootstrap and deployment](bootstrap.md) | Container Quadlet deploy, node-zero self-management, and adoption runbooks |
| [Running the daemon](serve.md) | `exocomp serve` flags, process lifecycle, signal handling, and single-instance locks |
| [Planned reboot & drain mode](drain-mode.md) | `exocomp drain` — cordoning the daemon, draining in-flight jobs, and reboot safety |
| [Upgrade and rollback](upgrade.md) | Zero-sudo upgrade choreography: mandatory pre-upgrade snapshots, binary swap, rollback |
| [Containers and derived images](containers.md) | Shipped OCI image, offline builds, and derived `FROM exocomp` custom containers |

## High availability and disaster recovery

| Runbook | Description |
|---|---|
| [Disaster recovery](disaster-recovery.md) | The 3 backup legs (NAS/S3/Git), recovery kit, and restoring onto a dead host |
| [Cold-standby failover](failover.md) | Replicated store takeover, Litestream fast-forward, and split-brain active lease guards |
| [Offline caches and vendoring](offline-caches.md) | Vendoring Ansible collections and OpenTofu providers for airgapped operations |

## Reference specifications

For full API and CLI references, see the [Technical reference](../reference/index.md).
