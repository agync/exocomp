# Running the daemon

`exocomp serve` (installed as `exocompd`) is the long-running half of the control plane:
one process holding the OpenTofu HTTP state backend on loopback, the job queue's workers,
and the scheduler. It is what the container runs: the image's `CMD` is `serve` with
`XDG_DATA_HOME=/var/lib`, and the Quadlet unit names the same thing explicitly
(`Exec=serve`) rather than relying on the image default.

```
exocomp serve --port 0 --drain-grace 30s --reload-interval 1m
```

At startup it sweeps the jobs a process that is no longer alive left `queued` or
`running` into `interrupted`, records the address its backend bound in
`<data dir>/state-backend.addr`, prints one line per subsystem it brought up, and warns
once per workspace still holding a state lock whose job is dead:

```
WARNING: workspace prod holds a stale state lock (apply by ci@lima since 2026-08-22 03:11:05Z); run `exocomp workspace unlock prod`
```

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--port` | `0` | Loopback port for the state backend; `0` takes an ephemeral one. The bound address is written to `<data dir>/state-backend.addr`, which is how a CLI finds it. The backend binds `127.0.0.1` only ([ADR 0004](../adr/0004-opentofu-http-state-backend.md)) — remote access is a reverse proxy's job, never a bind address. |
| `--drain-grace` | `30s` | How long jobs already running may finish after a signal before they are cancelled. Generous on purpose: a backup leg writes its own `backup_run` row at the end. |
| `--reload-interval` | `1m` | How often schedules are rebuilt from the database, so `schedule add/enable/disable` reaches a running daemon without a signal. `0` means only `SIGHUP` reloads. |
| `--sandbox` | `$EXOCOMP_SANDBOX`, else `enforcing` | Job isolation tier: `opportunistic`, `enforcing`, or `disabled`. See below. |
| `--secret-staging` | `auto` | Where a run-secret staged as a file (`bootstrap run --capture-file`) lands: `auto`, `tmpfs`, or `disk`. See below. |

## Job isolation

Every job subprocess runs under a kernel sandbox where the host supports one
([ADR 0005](../adr/0005-job-sandboxing-nono.md)). The tier is decided once at startup and
printed:

```
job isolation tier: enforcing
```

`enforcing` — the default — requires the sandbox: when the host cannot enforce it,
`exocomp serve` and `exocomp doctor` refuse rather than run the fleet's jobs unsandboxed.
That is secure-by-default, and it applies to the empty-flag default as much as to an
explicit `--sandbox enforcing`. `opportunistic` asks for the same sandbox but accepts plain
`disabled` when the host cannot enforce it: a degradation is a `WARNING` on stderr and a
line in `exocomp doctor`, never a silent downgrade. `disabled` skips the probe entirely,
which is the honest way to run on a kernel without Landlock. Pass `--sandbox opportunistic`
to accept the subprocess boundary on degradation, or `--sandbox disabled` for no sandbox at
all.

**Per-run privileged override.** The tier above is the daemon's default for every job. One
run can opt out of the sandbox for itself with `--privileged` (on `playbook run`, `run`,
`ansible`/`command`, and `run-cli`), or the "Run unsandboxed (privileged)" checkbox on a
source's web launcher: that one job runs at the `disabled` tier — no kernel policy, full host
access — even when the daemon default is `enforcing`. It is for trusted, operator-authored
work that genuinely needs host access, and it is operator-only (the web and API faces refuse
a readonly or approve token; the local CLI operator is already trusted), off by default, and
audited: the marker is persisted on the job row, an attributed ledger event records who asked,
and the queue records the unsandboxed execution. A privileged job can reach the data dir,
`host.key`, `keyring.age`, and the database, and mint tokens via the local CLI — the same
trusted-playbook posture the `disabled` tier documents, so never run an untrusted playbook
privileged (see [security/threat-model.md](../security/threat-model.md)).

The probe is an assertion, not a version check. It stands up a file standing in for
`host.key`, two listening unix sockets, and a listening loopback TCP port, runs a canary
inside a sandbox that grants none of them, and requires the refusals.

What each kind of job may reach:

| | Granted | Egress |
|---|---|---|
| ansible | the job's private dir, the playbooks and inventory its argv names, the vendored collections, the tool tree, `/dev/shm`, `~/.ansible` under the redirected home, the single file `/etc/passwd` (ssh resolves the local user through it) | TCP 22 |
| tofu | the job's private dir, the workspace directory, the provider cache, the tool tree | TCP 443 and the state backend's loopback port, plus any extra ports the workspace declared (below) |

Shell runbooks and backup legs still run at the `disabled` tier. The backup engine reads
`keyring.age` by design — it is what every target root gets a copy of — so a profile that
denied it would deny the job its own work. Sandboxing those kinds needs a profile shaped
around the backup engine's targets, which does not exist.

## Run-secret file staging

A run-secret a tool must receive as a file rather than an env value (`bootstrap run
--capture-file`, see [ansible-first-bootstrap.md](ansible-first-bootstrap.md)) is written
to a `0600` file inside a per-instance session directory,
`<root>/exocomp.<pid>.<rand>/`. `--secret-staging` chooses the root:

- **`auto`** (default) — stage onto `/dev/shm` (RAM, present unconditionally in every
  rootless podman container) when it is actually usable: present, backed by tmpfs, with
  adequate free space. Otherwise fall back to the disk root (`<data dir>/run/`, the same
  tree job private directories already use) — **loudly**: a `WARNING` in the daemon's own
  log at startup, and a job event on every run that actually lands a secret there. Never a
  silent downgrade.
- **`tmpfs`** — force `/dev/shm`; refuse to start if it is unusable. The strict "never a
  secret on disk" posture, enforced rather than merely attempted.
- **`disk`** — force the disk root explicitly.

Cleanup is three layers: the file is unlinked once the subprocess that read it exits; this
instance's whole session directory is removed on a graceful stop; and a startup sweep
removes any session directory whose owning process is no longer running — never one still
alive, so it can never touch a concurrent sibling exocomp instance's in-flight secrets on a
shared root.

The disk backend's residual (a secret's bytes on durable storage, and so potentially in
swap) is bounded the same way `/dev/shm` staging always was: best-effort `mlock` (a host
that forbids it still stages, unlocked, with the residual reported), `0600` files inside
`0700` session directories, and confirmation that `<data dir>/run/` sits outside every
backup's captured set — `internal/backup`'s staging tree is built by writing four named
files into a fresh temp dir, never by walking the data dir, so nothing under `run/` is ever
swept into a backup or a restore.

**A hard crash's residual differs by backend.** On `tmpfs`, a container restart clears
`/dev/shm` unconditionally, so a session directory a broken cleanup path left behind
cannot outlive the container even before the startup sweep runs. On `disk`, the same
session directory sits on durable storage until the daemon's next startup sweep — a
daemon that never restarts after such a crash leaves it there. The sweep also has a
bounded, acknowledged gap: it tells a live session directory from an orphaned one by
probing the owning pid, and a low pid space after a container restart can in principle
let a fresh process reuse a dead pid's number before the sweep runs, so a stale
directory reads as still-owned and is skipped. Best-effort, not eliminated — see
`internal/executor/pidalive.go`.

To get the strict "never a secret on durable disk" guarantee unconditionally, choose
`--secret-staging tmpfs` explicitly; `auto`'s disk fallback is loud (warning, job event,
doctor report) but is still a fallback, not a refusal.

`exocomp doctor` reports the resolved backend, the tmpfs usability probe's own verdict, the
disk residual posture when disk is in play, and the mlock posture under **run-secret file
staging**.

Denied everywhere: `exocomp.db`, `host.key`, `keyring.age`, `serve.lock`,
`state-backend.addr`, and every other job's private directory. Grants are checked against
those paths before a job starts, so a profile that would expose them fails the job rather
than running it.

**Egress is filtered by port, not by destination.** Landlock's network rules match the TCP
port and nothing else, and the only per-destination filtering nono offers routes traffic
through an HTTPS-terminating proxy — unusable for SSH and wrong for provider signature
verification. So an ansible job can open TCP 22 to any address, and a tofu job any TCP 443.
Restricting *which* hosts a job reaches stays a network-layer question.

### Extra tofu egress ports

A tofu run reaches TCP 443 and the state backend and nothing else, which is enough for the
registry and any provider that talks to an HTTPS API. A provider that must reach a control
plane on another port does not fit: the kubernetes and helm providers against a local
kubeadm cluster talk to its API server on 6443, and the sandbox denies it.

`exocomp workspace egress <name> <port>...` grants extra outbound ports to one workspace's
tofu runs, on top of the built-in grant:

```
exocomp workspace egress k8s-home 6443
```

The declaration is per workspace and empty by default: a workspace that has declared
nothing keeps exactly the 443-and-backend posture, and a grant on one workspace never
touches another's run. Ports are validated (`1..65535`) and de-duplicated when declared, so
a typo is refused at the CLI rather than reaching a run.

This is a deliberate widening, and its limit is the same one the section above states:
**Landlock filters egress by port, not by destination.** Granting 6443 permits a run to
open 6443 to *any* host, not only the intended cluster — the grant cannot be scoped to the
API server's address. Declare only the ports a workspace's providers genuinely need, and
keep host-level restriction a network-layer job. The grant also only means anything under
the sandbox: a fleet degraded to the `disabled` tier enforces no egress port limit at all,
so this declaration is inert there.

**A pathname unix socket is not covered.** Landlock mediates path opening, and
`connect(2)` on an `AF_UNIX` socket opens nothing, so no Landlock ABI shipped so far can
refuse it — abstract sockets, which Landlock does scope, are refused. The mitigation is
placement: exocomp's own surfaces are loopback TCP inside the data dir's recorded address,
no exocomp unix socket exists for a job to reach, and no podman socket is mounted into the
container. `exocomp doctor` prints this rather than implying a denial it cannot make.

**The data dir has to be short.** A sandboxed job's `TMPDIR` lives under
`<data dir>/run/job-<n>/`, and the tools that run there coordinate their workers over unix
sockets placed inside it. `AF_UNIX` addresses cap at 108 bytes, so `<data dir>/run` longer
than 57 bytes cannot support the tier; the probe checks it at startup and degrades loudly
with the measurement. The packaged path, `/var/lib/exocomp/run`, is 20.

## Signals

| Signal | Behaviour |
|---|---|
| `SIGHUP` | Rebuild every schedule registration from the database. A schedule added, edited, enabled, or disabled through the CLI takes effect here; the new set is built whole before it replaces the running one, so a row the parser rejects is logged and skipped without disturbing the rest. |
| `SIGTERM`, `SIGINT` | Shut down in the one order that leaves the database consistent: the scheduler first, so nothing new fires; then the jobs already running, which get `--drain-grace` to write their own terminal rows before anything cancels them; then whatever is still queued and owned by this process, retired to `interrupted`; then any other non-terminal row left by a process the kernel confirms is dead, retired the same way; the state backend last, because a tofu leg is still pushing state to it. |

A daemon that exits leaves no `queued` or `running` row behind, so there is nothing for
the next boot's sweep to repair. Work that outlasts the grace window is cancelled rather
than waited on forever, and a cancelled backup leg records `cancelled` — its
`backup_run` row is closed under a context the cancellation cannot reach.

For a *planned* reboot, do not lean on this grace window to cancel work: cordon the daemon
first with `exocomp drain`, let in-flight jobs finish on their own, and reboot once it
reports safe. See [Draining for a planned reboot](drain-mode.md).

## Schedules

`schedule add` validates `--cron` with the same parser the daemon registers with (six
fields, seconds first, or `@every`), so a row that could never fire is refused at add
time rather than logged on every boot. `--concurrency-key` is rejected together with
`--kind backup`: a backup fire keys itself per target (`backup:<target>`), so a key given
here would never be used.

At boot, and on every `SIGHUP` or `--reload-interval` tick, the daemon builds its whole
set of cron registrations from the `schedule` table before swapping it in. A row the
parser rejects — a schedule edited or restored into a state that no longer validates —
is logged and left out rather than aborting the daemon or every other schedule; the
previous, working set stays registered until a reload succeeds.

### Catch-up after downtime

Cron fires forward only, so a scheduled window that falls while the daemon is down would
otherwise be dropped in silence — a nightly backup or restore drill simply skipped until
its next natural occurrence. To close that gap the daemon records each schedule's
`last_run`. When it (re)loads, a schedule whose cron had at least one occurrence between
its `last_run` and now fires **once** to settle the miss, then resumes its normal forward
schedule.

The catch-up is coalesced, not replayed: a daemon down for a week fires one backup, never
one per missed night. A schedule that has never fired (`last_run` unset) does not
back-fire — it only ever schedules forward from now. A schedule whose `last_run` is recent
enough that nothing was missed does nothing.

Every fire — a normal cron fire and a catch-up alike — enqueues under an idempotency key
anchored on the occurrence's own scheduled instant, so a normal fire and a catch-up that
recovers the **same** occurrence collapse to one job. This closes the crash window between
a successful enqueue and the `last_run` stamp: if the daemon dies there, the next boot's
catch-up recomputes the identical key off the still-unadvanced `last_run` and the queue
returns the existing job rather than a second one. Two overlapping reloads racing the same
stale `last_run` collapse the same way. Conversely, `last_run` only advances once the fire
is safely enqueued: if the enqueue fails, `last_run` is left untouched so the occurrence is
still owed and catch-up re-fires it on the next tick or reboot — a transient enqueue error
delays a scheduled window, it does not silently drop it. For a fan-out schedule (a backup
with no explicit target), a partial failure withholds the stamp and the re-fire lets only
the target that failed through: the occurrence key dedups the ones that already succeeded.

## One daemon per data dir

Startup takes an exclusive `flock` on `<data dir>/serve.lock` and writes its pid there. A
second `exocomp serve` against the same data dir exits 1 naming the holder:

```
Error: serve: another exocomp serve (pid 1778339) already owns /var/lib/exocomp
```

Two daemons would fire every schedule twice and race for every queued job. The lock
belongs to the open descriptor, so it is released when the process ends however it ends —
a killed daemon leaves nothing to clean up, and the pid in the file is informational.

The `flock` is host-local: it says nothing about a daemon on **another** host sharing the
same replicated store. That case is guarded separately by the cross-host active lease — a
row in the store with a wall-clock TTL that the active daemon renews, and that a starting
daemon on another host refuses to override. See [cold-standby failover](failover.md); the
lease flags are `--lease-ttl`, `--lease-renew`, and `--force-lease`.

Acquiring the lock retries for 250ms on `EWOULDBLOCK` before giving up. A CLI checking
whether a daemon is up (below) takes only a shared, non-blocking probe of the same file
and releases it immediately, but a starting daemon and a probe can still land on the file
in the same instant; the retry is what keeps that instant from costing the daemon the
directory.

## What the CLI does while the daemon is up

`workspace plan`, `workspace apply`, `playbook run`, `playbook vendor`, and `backup run`
do not run jobs when a daemon is running. They detect one by two signals together — the
daemon lock is held (via the shared probe above), and the recorded backend address
answers `/healthz` — because either alone is ambiguous: a `workspace plan` on a
daemonless host serves a state backend of its own for the length of its run.

| Daemon | What the command does |
|---|---|
| up | Enqueues the job, prints `job <id> queued for the running daemon`, streams the job's events from the store as they land, and exits with the job's exit code. It never claims the row — `exocomp serve` claims and runs it. |
| down | Enqueues and drains the job in its own process, serving the state backend itself for the length of the run. A one-shot drain claims only the jobs it enqueued, never a daemon's rows, so it cannot run under a live daemon's driver by accident. |

A job row carries only public arguments. The secret material a run needs — a workspace's
backend bearer and state passphrase, an ansible job's vault password — is derived from
the envelope by whichever process runs the job, which is why a job records its
`vault_project` and never the password (AGENTS.md invariant 4). Events are redacted
before they are written, so the tail the CLI prints is already scrubbed.

If the daemon disappears while a command is following its job, the command stops with an
error naming the job rather than waiting on a row nothing will move.

## The browser surface

`--web-addr` serves a browser surface behind the ADR-0009 token→session exchange, with the
token's scope deciding what it may do. It is not view-only: an `operator`-scoped session
manages the daemon from the browser, not just the CLI.

- **Run** a source's playbook or an ad-hoc command from that source's page (each source has
  a launcher with a discovered-playbook list); **plan** and **apply** a workspace —
  each enqueues through the same service verbs the CLI uses and follows the job.
- **Sources**: add a path source, enable/disable, remove, or add a git source (which seals
  its deploy key and shows the public key once).
- **Secrets**: seal, rotate, delete. **Tokens**: mint, rotate, revoke.
- **Schedules**, **policies**, and **runners**: add and remove; schedules also enable/disable.
- **Approvals**: decide a pending job, as before.
- **Netgroups**, **checks**, and **rules** (rules read-only): manage netgroup
  and check rows; each check target shows its netgroup chip and how long ago
  its status last changed (`changed 3h ago`, or `unobserved`). The dashboard's
  Checks card shows the registered total and how many targets the sweep
  observes up; a job's "fired by rule <name>" line links to `/rules` and the
  event that raised the run; the events page filters by `?source=` and pages
  by `?limit=` (floor 100, cap 500). The dashboard's Schedules card shows the
  registered total and how many are disabled.
- **Approvals live-update**: the pending-approvals list subscribes to a
  browser-session SSE stream (`GET /approvals/stream`, the same
  `tailApprovals` the phone API serves) and reloads when the pending set
  changes, so the desk and phone never decide on a stale list. The page
  renders a fingerprint (`?fp=`) of the pending list it just drew; the stream
  compares it to its own connect-time state and sends the snapshot only when
  they differ, so a change between render and connect cannot be silently
  dropped and a matching fingerprint produces no reload-only event. The tail
  writes a keepalive comment every 30 seconds while the list is quiet, so an
  idle page is not killed by an intermediary's idle timeout. Concurrent
  streams are capped per process and per identity (8 total, 4 per token), and
  a refused or permanently-ended stream surfaces a visible "Live updates
  stopped" banner. The banner only appears once the stream is closed for good
  — a refused (over-cap) attach or an ended socket — never during a transient
  drop EventSource is already retrying on its own, and it clears on the next
  successful reconnect, so a quiet live list whose fingerprint matched never
  wears a stale banner.
- **Workspaces**: each runnable workspace row has a **Saved plans** button
  opening a modal of its saved plan artifacts, each applyable by id through the
  same plan-gate path as the CLI/API (a policied plan may still require
  approval). The row's own Apply is fresh-apply.
- **Destructive actions confirm**: Remove/Delete/Revoke forms (checks,
  netgroups, schedules, sources, policies, runners, tokens, secrets) submit
  through a shared confirm dialog rather than firing on the first click, and
  the hidden `confirmed=1` field is a real server-side gate, not just a UI
  affordance — a confirmed-less destructive POST reads 400.

The sensitive writes — sealing or rotating a secret, minting or rotating a token, sealing a
git deploy key — require re-entering your own access token to confirm. A cookie alone cannot
drive them: the confirm proves possession of the token secret, which an ambient cross-site
request never holds. A `readonly` session keeps the read-only view; `approve` keeps the
decide-only surface.

These browser writes are same-origin form posts authenticated by the session, distinct from
the machine `/api/v1`, which stays bearer-only and refuses an ambient cookie
([API](api.md)). A process serving the surface without an open envelope (no key material)
disables the routes that seal — secret add/rotate and git-source add — with a clear notice,
rather than failing at submit.

## Redaction freshness

The daemon resolves the known-secret set per job at claim time, so a secret
sealed while the daemon runs is scrubbed from the very next job's events — no
restart or reload required.

## Known limits

Several processes creating a brand-new database at the same instant can hit
`SQLITE_BUSY` setting the `journal_mode` pragma: a journal-mode change needs an
exclusive lock that the busy handler does not cover, unlike ordinary reads and writes.
Run `exocomp init` (or any single command) first so the database file exists before a
second process opens it, or retry on failure.

The executor keeps reading a job's output pipes for up to ~2s after its process exits,
to drain whatever was already written. A grandchild the job process left behind —
a backgrounded helper, an ssh control master — that still holds those pipes open loses
any output it produces after that window; the job's own exit code and events are
unaffected.

A job's `HOME` is redirected into its own private directory under the sandboxed tier, so
an operator's `~/.ansible.cfg` or `~/.terraformrc` does not apply to a queued run. That is
the intent — a job row is meant to record everything a run depended on — but it is a
behaviour difference from the `disabled` tier worth knowing before comparing two runs.
