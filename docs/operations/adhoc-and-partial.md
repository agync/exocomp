---
type: Guide
title: "Ad-hoc runs and the unreachable-tolerant outcome"
description: "The ad-hoc command and ansible verbs under the same governance as a play, and tolerating an unreachable roaming host instead of paging."
status: stable
tags: [operations, ansible, adhoc]
---

# Ad-hoc runs and the unreachable-tolerant outcome

`playbook run` is the batch verb. The two verbs here are the everyday ones an
operator reaches for between plays — a one-off command across a host pattern, and
any single module — plus the outcome that keeps a scheduled converge from paging
when a roaming machine is asleep.

## `exocomp command` — the everyday verb

```
exocomp command --host <pattern> [--shell] [--user-session] [--become] -- <cmd...>
```

Runs a bare command on every host a pattern matches, through the ansible
**command** module (no shell), the ad-hoc `ansible` binary rather than
`ansible-playbook`:

```
exocomp command --host web -- systemctl is-active nginx
exocomp command --host 'fedora:!nas' --become -- dnf -y upgrade
exocomp command --source homelab --host db -- pg_isready
```

An ad-hoc run from a registered source resolves that source's Python manifest like a playbook run
does ([Decision 0011](../decisions/0011-project-dependency-resolution.md)). When the checkout
declares `ansible-core`, the run uses the venv's `ansible`; otherwise it uses the `ansible` on
`PATH`, with the venv's libraries when the source declares any. A manifest that will not resolve
fails the run, as it fails a playbook's, and the web form has no source-less fallback. Collections
stay on the shared cache. With no source the run uses the `ansible` on `PATH`; when that is missing
the job fails naming the `PATH` it searched, so a host install whose service manager starts the
daemon with a minimal `PATH` needs `ansible` on it.

`--host` takes any ansible pattern: a host, a group, or an expression like
`fedora:!nas`. `--limit` is an accepted alias. Everything after `--` is the
command; it is joined into the module's single argument and never spread onto
ansible's own flags. A command that needs a pipe or a redirect uses `--shell`,
which switches to the **shell** module:

```
exocomp command --host web --shell -- 'journalctl -u nginx | tail -50'
```

Commands targeting user-level services (`systemctl --user`, `dconf`, D-Bus desktop
services) use `--user-session` to automatically resolve and inject the active user's
`$XDG_RUNTIME_DIR` (`/run/user/<uid>`) and `$DBUS_SESSION_BUS_ADDRESS`:

```
exocomp command --host workstation --user-session -- systemctl --user status podman
```

For scripts and agents, `--output json` emits a structured, machine-parsable per-host
dictionary (`command.result`) rather than raw streaming terminal output:

```
exocomp command --host 'web*' --output json -- hostname -f
```

The command module runs no shell, so a pipe is an explicit `--shell`, never an
accident. Local execution without an explicit `--source` resolves relative to the
operator's home directory. `--become` works the same on every target, the daemon's own host
included, and needs no other flag. On a local connection under the sandbox the escalation's
`sudo` runs inside it and is refused ([playbook run](playbook-run.md)), while a target reached
over SSH, the daemon's host included, escalates on the far side.

## `exocomp ansible` — any module

```
exocomp ansible --host <pattern> -m <module> [-a "<args>"] [--become]
```

The power form for any module — `service`, `copy`, `package`, `ping`, `setup`, …
`command` is the ergonomic shorthand for `-m command`.

```
exocomp ansible --host web -m ansible.builtin.service -a "name=nginx state=restarted" --become
exocomp ansible --host all -m ansible.builtin.ping
exocomp ansible --host db -m ansible.builtin.setup -a "filter=ansible_distribution*"
```

## Same governance as a play

Both verbs run through the one queue with the same credential and isolation model
[`playbook run`](playbook-run.md) uses:

- **From a source checkout** with `--source <name>`, so the repo's `ansible.cfg`
  and inventory apply. `--inventory` names a registered inventory or a path; left
  empty, ansible discovers its own inventory (the checkout's `ansible.cfg`, then
  its default hosts file) — no localhost is pinned.
- **Sandboxed and redacted**, the vault-id from `--vault-project`, the run-secret
  set from `--run-secrets`, resolved from the envelope at run time.
- **`--ssh-identity <path>`** grants read access to one ssh identity key file — never
  its directory — on `command` and `ansible`, so a one-off against a key-only host does
  not need `--privileged`. A sandboxed run whose ssh key read is denied appends a hint
  naming this flag (see [playbook-run.md](playbook-run.md)).
- **Secrets never on argv.** The vault password and run-secret / dynamic-inventory
  credentials keep their out-of-argv paths (a private-dir file, named env). What a
  run *does* is public and rides the job row; what it *authenticates with* does
  not.
- **`--require-approval`** gates a destructive one: the job lands in
  `awaiting_approval` where no worker can claim it, an approval is created, and
  `exocomp approval link <id>` prints its decision link. It reuses the same gate a
  policy-gated tofu apply does.
- **On the daemon when a remote is selected.** With `--remote` or `EXOCOMP_REMOTE`, both
  verbs enqueue on that daemon (`POST /api/v1/adhoc/run`) and follow the job there:
  `--source` and `--inventory` resolve on the daemon and the source's default egress
  applies, as for a run started on that host ([Driving a remote
  daemon](remote-access.md#writes)).

### How a run reads in the web jobs list

The web UI's jobs list names an ad-hoc run by what it ran. The first line is the
module and the host pattern (`command · host-a`), taken from the job's argv by flag name.
The second line, in monospace, is the module args or command, clipped to one line with
the full text on hover and in the job detail's Command tile. A `run-cli` job keeps its
`cli:<tool>` title and shows the tool arguments on that second line. Source and check-mode
facts stay on the right of it. On a phone the command line sits under the title.

The command text is shown only to an operator token. Before it is rendered, every
sealed secret value is scrubbed from it, the same filter the job log applies. If the
surface cannot open every sealed secret, it shows no command text at all, so a read-only
or approve token and a degraded envelope both see the title alone. The scrub covers the
list row and the Command tile. The Schedule and Rerun dialogs on the job detail still show
the argv as recorded to an operator, since both replay it. `GET /api/v1/jobs` is
unchanged: it still omits args.

### Injection posture

The host pattern, the module name, the module args, and the command are guarded
before the run starts. A pattern or command that **starts with a dash** (it would
be read as a flag) or carries a **control character** (it would split or truncate
the single argv token) is refused; a module name must be the plain
`namespace.collection.module` shape. A flag's value is one argv token after its
flag and cannot introduce a second flag — the guard is against a value that would
truncate or split the argument, the same posture the passthrough flags carry.

## The unreachable-tolerant outcome

A static fleet with a **roaming laptop** in the daily target is the motivating
case: a scheduled `site.yml` fails and pages every time the laptop sleeps, because
an unreachable host is a non-zero exit and correctness is decided by exit code
(AGENTS.md invariant 3). `ansible-playbook` exits 4 when a host is unreachable —
the same failure as any other non-zero exit.

`--tolerate-unreachable` is a per-run (and per-schedule) **opt-in** that keeps the
strict default for everyone who does not ask:

```
exocomp playbook run site.yml --inventory prod --tolerate-unreachable
exocomp command --host all --tolerate-unreachable -- uptime
```

When it is set, the run is classified by its record — a playbook's `PLAY RECAP`,
or an ad-hoc run's `host | STATUS` lines (ad-hoc mode prints no recap):

- **Only unreachable hosts** were the non-success (`failed=0` on every host the
  run reached, and at least one host WAS reached) → the run settles in the
  **`degraded`** terminal state: the run completed, every reachable target
  succeeded, the offline ones were skipped because the launch tolerated it. It
  does **not** page, and the CLI exits 0 for it (a tolerated offline host is not
  a caller failure). The reachable hosts converged; the offline ones are
  recorded and named in a job event. The `partial` column is the legacy marker
  for this outcome, still written beside the state for readers that predate it.
- **Every addressed host unreachable** (the whole inventory, or the whole
  `--limit` set, was offline) → a **hard failure**, not degraded: with nothing
  reached, nothing converged, and the tolerance flag must not turn a full miss
  into a tolerated success.
- **Any reachable host's task failed** → a **hard failure**, exactly as without
  the flag. The tolerance never hides a real failure.

The job's true exit code is still recorded (a degraded job keeps its `exit 4`);
`--tolerate-unreachable` only changes how the queue classifies it, not the number.
A run that errors before any host record is **not** called degraded — with nothing
to prove `failed=0`, a tolerated degradation must be proven, never assumed.

The degraded job carries a warning-level event and the structured outcome under
`ansible_partial` in its `raw_json`:

```
ansible: degraded — 1 host(s) unreachable, tolerated: laptop (reachable hosts converged)
```

```json
{"ansible_partial": {"operation": "run", "unreachable": ["laptop"]}}
```

`executor.AnsiblePartialHosts(rawJSON)` reads it back out, the twin of
`AnsibleCheckChanges` for the config-drift outcome — a reader turns a degraded
fire into the list of offline hosts.

## The homelab example — a converge that does not page at night

Register the recurring converge over a standing inventory with the tolerance on:

```
exocomp schedule add nightly-converge --kind play --tolerate-unreachable --source homelab \
  --target tf:homelab --cron "0 0 3 * * *" -- site.yml
```

Every fire converges the online fleet. When only the laptop is offline the fire
settles in the **`degraded`** state — the online hosts converged, the laptop
named — instead of failing and paging; the degraded state is still surfaced, so
a machine that is *always* offline is visible rather than silently skipped. The
fail-closed rule for play schedules still holds: a missing, stale, or hostless
standing inventory fires nothing rather than reporting a clean sweep against
nobody. `--tolerate-unreachable` is rejected on any kind but `play`.

A genuine breakage — a task that fails on a host that *is* online — still fails the
fire and pages, tolerance or not. The flag narrows exactly one thing: an offline
host is no longer a failure.

Related: [Playbook run flags and config-drift](playbook-run.md) ·
[Sources and credentials](sources.md) ·
[Provision → onboard → converge](provision-converge.md) ·
[Reacting to alerts](reactive.md).
