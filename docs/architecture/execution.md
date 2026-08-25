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
| `disabled` | **shipped**, and the only topology bootstrap and DR require | subprocess boundary, rootless container, per-job 0700 private dir, env allowlists |
| `enforcing` | **shipped** (`nono`, Landlock), the **default** on Linux | the above plus a kernel default-deny sandbox per job subprocess |
| `container` | designed, deferred alongside Wails and OpenBao | per-job sibling containers |

Selecting a tier is `--sandbox`/`$EXOCOMP_SANDBOX`, defaulting to `enforcing`. A third
name, `opportunistic`, is not an isolation mechanism of its own: it asks for `enforcing`
and accepts a loud degradation to `disabled` where the host cannot deliver the sandbox.

Under `disabled` a job is trust-equivalent to the daemon: isolation is process boundary and
file permissions, not kernel policy, so anything a job subprocess's uid can reach, the
daemon's own code could reach too. `enforcing` is what breaks that equivalence, and it is
the default wherever the kernel supports it — a host that cannot deliver it fails closed at
startup rather than running the fleet's jobs unsandboxed.

## `disabled`

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

## `enforcing`

Every job subprocess runs under `nono run` (Landlock on Linux, Apache-2.0, ≥ 0.55.0
sha256-pinned) with a default-deny filesystem policy built per job kind. The sandbox is
spliced in *underneath* the ansible and tofu drivers: it sees the argv and environment
those drivers finished building, and hands the result to the same local subprocess driver,
so streaming, process groups, and exit-code semantics have one implementation.

| | Paths |
|---|---|
| Allowed | the job's own private directory, the working directory, the repo paths its argv names, the tool tree, the provider and collection caches |
| Denied | `exocomp.db`, `host.key`, `keyring.age`, `serve.lock`, `state-backend.addr`, every other job's private directory |

Every job gets a private directory of its own, and the driver's secret material — the
vault password, the backend bearer — is written there rather than into a root shared by
all jobs, so one job's grant never covers another's.

The denial is enforced twice: by the kernel, because nothing outside the profile is
granted, and by a guard that refuses to build a profile whose grants would cover the data
dir or any of the files above. That second check is the one that matters in practice —
the caches a job legitimately needs all live *inside* the data dir, so the mistake to
catch is a grant one directory too high.

Ansible and tofu jobs run under it. Shell runbooks and backup legs do not: the backup
engine reads `keyring.age` by design — it is the file every target root gets a copy of —
so those kinds need a profile shaped around the engine's targets rather than the deny list
above.

Network is scoped per kind: ansible gets TCP 22, tofu gets TCP 443 plus the loopback state
backend's port. **Scoped by port, not by destination**:
Landlock's network rules match the port alone, and the only per-destination filtering
available routes traffic through an HTTPS-terminating proxy, which is unusable for SSH and
wrong for provider signature verification.

`exocomp doctor` reports the launcher version against the floor, the kernel's Landlock ABI
(network rules need ABI v4+, kernel ≥ 6.7), the tier in effect, and what the startup probe
asserted — and under `opportunistic` the daemon degrades **loudly** to `disabled`, never
silently, on a kernel or a launcher that cannot deliver it. `enforcing` — the secure default
— is a policy, not a preference: an `enforcing` request (including the empty-flag default)
that cannot be enforced makes `serve` and `doctor` refuse rather than degrade, since running
the fleet's jobs unsandboxed is not what was asked for.

The probe asserts rather than assumes: it stands up a file standing in for `host.key`, two
listening unix sockets, and a listening loopback TCP port, then runs a canary inside a
sandbox granting none of them and requires the refusals. Live targets are the point — a
refusal has to come from the kernel policy, not from there being nothing to reach.

**A `connect(2)` to a pathname `AF_UNIX` socket is not mediated by any Landlock ABI shipped
so far**: Landlock hooks path opening, and connecting to a unix socket opens nothing.
Abstract sockets, which Landlock does scope, are refused; pathname ones are not. The socket
denial in [ADR 0005](../adr/0005-job-sandboxing-nono.md) therefore holds by placement rather
than by policy — exocomp ships no unix socket for a job to reach, its own surface is
loopback TCP, and no podman socket is mounted into the container — and `doctor` prints the
gap instead of implying a denial it cannot make. This is why the probe asserts against live
targets rather than assuming kernel behavior: an assumption here would have hidden exactly
this gap.

The consequence that matters is unchanged: a playbook is arbitrary code from a git
repository, and under this tier it cannot read the database, the host key, or the keyring.
That is most of the `container` tier's prize with no podman socket handed to `exocompd`
and no registry on the DR path — Landlock is local and unprivileged, so the sandbox
applies inside a rootless container and during a restore.

Two operational constraints come with it. A job's `HOME` is redirected into its own
private directory, so an operator's `~/.ansible.cfg` or `~/.terraformrc` does not reach a
queued run. And `<data dir>/run` must stay under 57 bytes: the tier redirects `TMPDIR`
beneath it, the tools that run there coordinate over unix sockets placed inside it, and
`AF_UNIX` addresses cap at 108. The probe measures it at startup and degrades loudly
rather than letting each job discover it as a timeout.

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
  DR-path runbooks are `disabled`-only, enforced by the doctor.
- Per-project tool versions widen the golden-file test matrix, so EEs are tested
  per-project, not globally.

With `enforcing` shipped, the remaining justification for this tier narrows to
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

A saved plan is the least-protected place a secret appears: it carries every attribute of
every resource the apply would change, in the clear, and it outlives the run that made it.
So it is encrypted at rest — `age` to the keyring's recipients, the same recipients that
wrap the DEK — the moment the run that wrote it ends, and the `artifact` row records the
ciphertext's path and digest. An apply decrypts it into the run's own 0700 private
directory and removes it when the run ends; the approval diff reads it the same way.
Plan artifacts are not part of the backup staging tree, so nothing about the restore drill
changes.

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
