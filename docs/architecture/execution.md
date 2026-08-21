# Execution and isolation

The queue executes every job through one `ExecutorDriver` interface, present from v0.1. The
interface costs approximately nothing to introduce early — the queue already isolates
execution behind a single call — and it is what makes the isolation tier a deployment
choice rather than a rewrite.

Three tiers, in increasing isolation and increasing cost:

| Tier | Status | Isolation mechanism |
|---|---|---|
| `local` | fallback, and the only topology bootstrap and DR require | subprocess boundary, per-job tmpfs, separate executor UID, rootless container |
| `local-sandboxed` | **default on Linux** where the kernel supports it | the above plus a kernel default-deny sandbox per job subprocess |
| `container` | opt-in, deferred post-v1 | per-job sibling containers |

## `local`

Pinned tools in the image, one subprocess per job, a per-job tmpfs directory
(mode 0700, unmounted at job end) holding decrypted secret material, a separate executor
UID, all inside the rootless container.

One image plus one volume is a working system. Nothing else has to be pulled at restore
time — which is exactly why this tier, and not a fancier one, is what the DR path is
allowed to use.

## `local-sandboxed`

Every job subprocess runs under `nono run --profile job-<kind>` (Landlock on Linux,
Apache-2.0, ≥ 0.55.0 sha256-pinned) with a default-deny filesystem policy.

| | Paths |
|---|---|
| Allowed | job tmpfs, repo checkout, artifact dir, tool paths, provider and collection caches |
| Denied | `exocomp.db`, `host.key`, the keyring, other jobs' tmpfs, **the exocomp API socket, and any podman socket** |

Network is scoped by runbook kind: Ansible gets SSH to inventory hosts; OpenTofu gets
provider endpoints plus the `127.0.0.1` state backend.

The consequence that matters: a playbook is arbitrary code from a git repository, and under
this tier it can no longer read the database, the host key, or the API socket. That is most
of the `container` tier's prize with no podman socket handed to `exocompd` and no registry
on the DR path — Landlock is local, so the sandbox still applies during a restore.

`exocomp doctor` probes the Landlock ABI (network rules need ABI v4+, kernel ≥ 6.7) and
degrades **loudly** to `local` on older kernels, never silently. It also asserts the
socket denial by attempting a connect from inside a probe sandbox and expecting failure.
Full rationale and the CVE lesson behind that probe:
[ADR 0005](../adr/0005-job-sandboxing-nono.md).

## `container`

Per-job sibling containers: Ansible via ansible-runner's native podman process isolation
and execution environments — the AWX EE pattern, and a capability of a dependency already
shipped rather than new orchestration — and plain `podman run` for OpenTofu.

What it buys: privilege separation with per-project EE images carrying independent
ansible and collection pins, and hard per-job memory caps.

The trades are stated so the choice is informed, not assumed:

- `exocompd` must mount the podman socket, which is effectively host-user power. The
  sensitive capability **moves rather than disappears**.
- EE images become a restore-time dependency. Any EE used by a runbook on the DR path must
  be rebuildable from the ops repo or mirrored in the data volume. The default is stricter:
  DR-path runbooks are `local`-only, enforced by the doctor.
- Per-project tool versions widen the golden-file test matrix, so EEs are tested
  per-project, not globally.

With `local-sandboxed` shipped, the remaining justification for this tier narrows to
per-project EE versioning and hard per-job resource caps, so it stays deferred alongside
Wails and OpenBao.

## Offline-restorable job dependencies

An executor is only as restorable as what it needs to fetch. Disaster recovery must not
depend on registry.opentofu.org or Ansible Galaxy being reachable:

- `TF_PLUGIN_CACHE_DIR` plus a filesystem provider mirror live in the data volume,
  populated on first `init` and refreshed by a scheduled job. `.terraform.lock.hcl` is
  respected, and the mirror is part of every backup.
- Galaxy collections and roles are vendored into the data volume the same way — including
  `community.sops`, which the design itself depends on.
- The restore drill asserts that a play and a plan both run **with network egress
  disabled**.

## Boot-time reconciliation

A container restart kills `ansible-playbook` mid-run, and a restored snapshot contains such
rows by construction. On startup, jobs in `queued` or `running` whose process is gone are
swept to `interrupted` — a terminal state, notified, **never auto-retried** — and stale
`tflock` rows whose owning job is dead are flagged with an audited one-click force-unlock.
The restore drill asserts both.
