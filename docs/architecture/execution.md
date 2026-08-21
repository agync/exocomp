# Execution and isolation

The queue executes every job through one `ExecutorDriver` interface, present from v0.1. The
interface costs approximately nothing to introduce early — the queue already isolates
execution behind a single call — and it is what makes the isolation tier a deployment
choice rather than a rewrite.

## Queue semantics

One `job` table, one daemon claiming whatever reaches it. `exocomp serve`'s queue claims
every queued row, whichever process enqueued it, and never idles. A one-shot CLI command
(`workspace plan` with no daemon up, for instance) runs its own queue instead, and that
queue's `Drain` claims only the rows it enqueued itself — a CLI never touches a daemon's
rows, so it cannot run a job through a driver built for a different job's kind by racing a
daemon that got there first.

Concurrency keys are enforced by a single SQL `UPDATE` at claim time
(`store.ClaimJob`) — the key check and the state transition to `running` are one
statement, conditioned on no other `running` row sharing the key — not by a map held in
process memory. A daemon and a CLI claiming from the same database file are both bound by
the same statement, so which one wins is a database fact, not a race either has to reason
about.

A job's subprocess runs in its own process group (`Setpgid`); cancelling a job signals the
whole group (`SIGKILL` to `-pid`), not just the process the executor started, because a
shell that exec'd something else or forked a helper is only reachable by group.

Three tiers, in increasing isolation and increasing cost:

| Tier | Status | Isolation mechanism |
|---|---|---|
| `local` | **shipped**, and the only topology bootstrap and DR require | subprocess boundary, rootless container, per-job 0700 private dir, env allowlists |
| `local-sandboxed` | designed, M3 (`nono`, Landlock) | the above plus a kernel default-deny sandbox per job subprocess |
| `container` | designed, M3, deferred alongside Wails and OpenBao | per-job sibling containers |

Until the sandbox tier lands, a job is trust-equivalent to the daemon: `local` isolates
jobs from each other and from the host by process boundary and file permissions, not by a
kernel policy, so anything a job subprocess's uid can reach, the daemon's own code could
reach too.

## `local`

Pinned tools in the image, one subprocess per job, run in its own process group
(`Setpgid`) so a cancel or timeout signals the whole tree rather than one pid a shell may
have exec'd past. Secret material — a workspace's backend bearer and state passphrase, an
ansible job's vault password — is written only to a per-job private directory
(`os.MkdirTemp`, mode 0700, removed when the job ends) inside the rootless container, and
the subprocess's environment is built from an allowlist rather than inherited, so an
operator's exported `TF_CLI_ARGS` cannot rewrite the argv a job row recorded.

This tier ships without a per-job tmpfs mount or a separate executor UID: every job
subprocess runs as the same container user, distinguished only by its own private
directory and the argv it was given. One image plus one volume is a working system.
Nothing else has to be pulled at restore time — which is exactly why this tier, and not a
fancier one, is what the DR path is allowed to use.

## `local-sandboxed`

Designed for M3: every job subprocess would run under `nono run --profile job-<kind>`
(Landlock on Linux, Apache-2.0, ≥ 0.55.0 sha256-pinned) with a default-deny filesystem
policy.

| | Paths |
|---|---|
| Allowed | job tmpfs, repo checkout, artifact dir, tool paths, provider and collection caches |
| Denied | `exocomp.db`, `host.key`, the keyring, other jobs' tmpfs, **the exocomp API socket, and any podman socket** |

Network would be scoped by runbook kind: Ansible gets SSH to inventory hosts; OpenTofu
gets provider endpoints plus the `127.0.0.1` state backend.

The consequence that matters: a playbook is arbitrary code from a git repository, and under
this tier it would no longer be able to read the database, the host key, or the API
socket. That is most of the `container` tier's prize with no podman socket handed to
`exocompd` and no registry on the DR path — Landlock is local, so the sandbox would still
apply during a restore.

`exocomp doctor` would probe the Landlock ABI (network rules need ABI v4+, kernel ≥ 6.7)
and degrade **loudly** to `local` on older kernels, never silently, and would assert the
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

Once `local-sandboxed` ships, the remaining justification for this tier narrows to
per-project EE versioning and hard per-job resource caps, so it stays deferred alongside
Wails and OpenBao.

## OpenTofu runs

A tofu job is one `Spec` of kind `tofu` whose argv is the operation plus the operator's own
arguments — everything a job row is allowed to remember. The driver supplies the rest per
run:

- a `terraform { backend "http" {} }` override file in the workspace directory, and the
  values that fill it — address, `LOCK`/`UNLOCK` methods, the workspace bearer — in a 0700
  private directory handed to `tofu init -backend-config`. The bearer never reaches the
  workspace directory or the process table.
- `TF_DATA_DIR` inside that private directory. `.terraform/terraform.tfstate` records the
  backend configuration verbatim, bearer included, so it dies with the run.
- `TF_ENCRYPTION` naming the `pbkdf2` key provider with a passphrase derived per workspace.
  Tofu encrypts its own state; `exocompd` stores the envelope it is handed
  ([ADR 0004](../adr/0004-opentofu-http-state-backend.md)).
- an environment built from an allowlist. `TF_VAR_` names pass through; other `TF_` names
  do not, because `TF_CLI_ARGS` would let whatever the operator exported rewrite the argv
  the job row records.

`init` runs before every operation, and a failing init is the job's exit code: the
operation never runs against a half-configured backend. `plan` runs with
`-detailed-exitcode`, where exit 2 means changes are present — neither success nor failure,
so the driver records a plan outcome in `job_event` and reports exit 0.

Two runs cannot share a workspace directory: they would remove each other's backend
override mid-flight. The queue's `tofu:<workspace>` concurrency key keeps them apart, and
the backend's own lock row holds against anything the queue does not know about.

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
swept to `interrupted` — a terminal state, notified, **never auto-retried**.

Stale `tflock` rows are handled the same way, and it is implemented, not aspirational: the
boot sweep runs first, then `exocomp serve` warns once per workspace whose lock still
names a job that is gone or terminal — a lock naming no live job row is stale by policy,
whether that job never existed, finished, or was swept above. `exocomp workspace unlock
<name>` breaks the lock, refusing outright while the job it names is still running (the
lock is the system working, not a leftover), and records the break as an audited
`job_event` system entry on the job the lock named, or on the workspace's most recent job.
The restore drill asserts both the job sweep and the lock flag.
