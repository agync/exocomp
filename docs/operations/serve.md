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

## Signals

| Signal | Behaviour |
|---|---|
| `SIGHUP` | Rebuild every schedule registration from the database. A schedule added, edited, enabled, or disabled through the CLI takes effect here; the new set is built whole before it replaces the running one, so a row the parser rejects is logged and skipped without disturbing the rest. |
| `SIGTERM`, `SIGINT` | Shut down in the one order that leaves the database consistent: the scheduler first, so nothing new fires; then the jobs already running, which get `--drain-grace` to write their own terminal rows before anything cancels them; then whatever is still queued and owned by this process, retired to `interrupted`; then any other non-terminal row left by a process the kernel confirms is dead, retired the same way; the state backend last, because a tofu leg is still pushing state to it. |

A daemon that exits leaves no `queued` or `running` row behind, so there is nothing for
the next boot's sweep to repair. Work that outlasts the grace window is cancelled rather
than waited on forever, and a cancelled backup leg records `cancelled` — its
`backup_run` row is closed under a context the cancellation cannot reach.

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

## One daemon per data dir

Startup takes an exclusive `flock` on `<data dir>/serve.lock` and writes its pid there. A
second `exocomp serve` against the same data dir exits 1 naming the holder:

```
Error: serve: another exocomp serve (pid 1778339) already owns /var/lib/exocomp
```

Two daemons would fire every schedule twice and race for every queued job. The lock
belongs to the open descriptor, so it is released when the process ends however it ends —
a killed daemon leaves nothing to clean up, and the pid in the file is informational.

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
