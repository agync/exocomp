---
type: Guide
title: "Driving a remote daemon"
description: "Login, the default remote and --local: certificate pinning, precedence, pinned reads, writes that enqueue on the other box, the commands that stay local, following a job live, and the inputs that do not cross the wire."
status: stable
tags: [operations, remote, cli]
---

# Driving a remote daemon

`exocomp login` pins a remote daemon's certificate and a bearer token so the same CLI can
drive a box other than the one it runs on. The remote the first `login` records becomes the
default, and from then on commands go to that box's `/api/v1` instead of the local data dir;
`--remote` names another pinned box, and `--local` keeps one command on this machine. Reads render the daemon's own projections; writes
enqueue there and the job can be followed live. The transport is the [Decision 0009](../decisions/0009-lan-access-tokens.md)
TLS listener ([LAN access](lan-access.md)) with the certificate pinned trust-on-first-use.

## Pinning a remote (TOFU)

```
exocomp login server.tailnet.ts.net:8443 --token exo_...
exocomp --remote server jobs list
```

`login` connects over TLS, captures the daemon's self-signed certificate SHA-256 fingerprint
(the same hex `doctor` and the web login screen print), shows it, and — on confirmation —
records `{endpoint, fingerprint, token}` under an alias in `~/.config/exocomp/hosts.toml`
(`0600`, atomic write via temp file + rename), the same trust-on-first-use discipline as
`~/.ssh/known_hosts`. Later calls verify the pin and refuse to connect if it changed.

`--name` sets the alias explicitly; otherwise it derives from the endpoint's first host label
(`server.tailnet.ts.net` → `server`). The first login becomes the default remote unless
`--no-default` is passed or a default is already set.

A first-time pin asks for interactive confirmation (`y/N`) unless `--assume-yes` is given; a
non-interactive session with no `--assume-yes` refuses rather than blocking on stdin. A raw
HTTP endpoint is rejected everywhere in the client (`must be https`): a bearer token over
cleartext is sniffable, so there is no insecure remote path to opt into.

### Replacing a changed pin

If the alias is already pinned to a *different* fingerprint, `login` refuses and prints both
values — either an out-of-band re-key or a machine-in-the-middle. Accepting the change (or
re-pointing an alias at a different endpoint) takes `--replace-pin`, and nothing else does
it. `--assume-yes` pins a *first* fingerprint on non-interactive stdin but never replaces
one. That split is deliberate: every script, CI step, and automated caller must pass
`--assume-yes` to pin at all, so if it also replaced pins, the one invocation automation
always uses would re-pin to whatever certificate answered and hand it the bearer token (see
the [threat model](../security/threat-model.md)). Confirm the new fingerprint out of band
first; a refusal exits non-zero and leaves `hosts.toml` untouched.

## Selecting a remote: flags, env, precedence

Every command inherits `--remote`, `--local` and `--token`. The first match decides where a
command goes ([Decision 0026](../decisions/0026-remote-first-cli.md)):

| Input | Where the command goes |
|---|---|
| `--local` | this machine's data dir, whatever else is configured; with `--remote` it is an error |
| `--remote <selector>` | that remote; `--remote=` (empty value) is the `hosts.toml` default |
| `EXOCOMP_REMOTE` | that remote, when set and not empty |
| the `hosts.toml` default | that remote, with no flag at all |
| none of these | this machine's data dir |

`<selector>` is either a stored alias (`server`) or a raw `https://` endpoint pinned under
some alias by an earlier `login` — an endpoint that was never logged in is refused (TOFU
demands the pin exist first). The first `login` makes its remote the default unless
`--no-default` is passed, so from then on every command goes there. `--local` is the one way
to act on this machine's data dir while a default is stored; an empty `EXOCOMP_REMOTE` counts
as unset. A `hosts.toml` that cannot be read fails the command with an error naming
`--local`, rather than guessing.

A `hosts.toml` written by an earlier version may already name a default, set by the first
`login` when a default only filled a valueless `--remote=`. It now applies to every command.
On a host that should stay local, such as a daemon host that logged in to itself for
`--remote self drain`, remove the `default` line, or log in with `--no-default`.

The bearer token resolves the same way `login` resolves it: `--token` flag → `EXOCOMP_TOKEN`
→ the token `login` stored for that alias. `--token`/`EXOCOMP_TOKEN` let one invocation use a
different-scoped token than the one pinned, without a re-login.

```
exocomp --remote server jobs list --output json
EXOCOMP_REMOTE=server EXOCOMP_TOKEN=exo_... exocomp doctor
exocomp --remote https://server.tailnet.ts.net:8443 workspace list
```

`--remote` is deliberately not `--host`: `--host` already names the ansible host *pattern* on
`command`/`ansible` (which managed nodes a play targets) — a different axis from which daemon
the CLI itself talks to. Reusing it would conflate the two.

## Reads

The client (`internal/client`) re-renders the daemon's `wire` bytes exactly: `--output json`
in remote mode is byte-identical to `--output json` run locally against the same resource.

| Verb | Behavior |
|---|---|
| `exocomp doctor` | the **remote** daemon's health summary, not this workstation's |
| `exocomp jobs list` / `get` / `events` / `follow` | the remote daemon's job state |
| `exocomp approval list` | pending approvals on the remote daemon |
| `exocomp source list`, `workspace list` | the remote daemon's registrations |
| `exocomp drain`, `drain interrupted` | cordons the remote daemon ([drain mode](drain-mode.md)) |
| `exocomp reload`, `restart` | reloads or restarts the remote daemon |

## Writes

Writes enqueue on the remote daemon and return its own envelope.

| Command | Over `--remote` |
|---|---|
| `playbook run` | Enqueues, then follows the job to its end and exits with the job's code. `--no-follow` prints the id and exits 0 |
| `command`, `ansible` | Enqueues the ad-hoc module run, then follows it to its end and exits with the job's code. `--source` resolves on the daemon, so the run uses that source's checkout, `ansible.cfg`, inventory and default egress there. A run with no `--source` runs in the daemon's job scratch dir, where a local run uses this machine's home directory. `--require-approval` prints the pending approval and the `approval decide` command instead |
| `workspace plan` | Enqueues, prints the job id |
| `workspace destroy` | Enqueues, prints the job id |
| `workspace apply` | Enqueues, prints the job id — or the pending approval, its policy, deadline and TTL when the plan's verdict gates it |
| `approval decide` | Settles the approval on the daemon, which derives the capability token itself. Needs an **`approve`-scope** token: `operator` may enqueue but may not settle what it enqueued, and no token may decide an approval for a job it asked for itself |
| `jobs follow <id>` | Streams the job's events and exits with the job's code |
| `source add`, `rm`, `sync`, `egress`, `run-secrets` | Changes the source on the daemon. A `--path` names a tree on the daemon's host; `--allowed-signers` is read here and its content sent. A git source prints the deploy key's public half the daemon generated |
| `secrets add`, `replace`, `rm`, `list` | Seals on the daemon. The value is read from stdin here and sent over the pinned connection; no answer carries it. `rm` of a secret a binding still maps needs `--force` or `--assume-yes`, as locally |
| `run-secret add`, `rm`, `list` | Changes the bindings on the daemon |
| `schedule add`, `rm`, `list`, `enable`, `disable`; `rule add`, `rm`, `list`, `enable`, `disable` | Changes the daemon's schedules and rules, checked there as a local add is. A play schedule takes its playbook and `--check`; ansible options after the playbook are set on the daemon's host with `--local`. A rule's `--ssh-identity` names a file on the daemon that its operator allowed. `rule list` shows a shell action's kind, not its argv, which no readonly answer carries. `exocomp reload` applies a schedule change at once |
| `silence add`, `rm`, `list`; `mute add`, `rm`, `list` | Holds or lifts on the daemon, which stamps the row as created over the API. `--for` is sent as a duration and the daemon counts it from its own clock |
| `check add`, `update`, `enable`, `disable`, `rm`; `netgroup add`, `update`, `rm`, `list` | Changes the daemon's checks and netgroups, validated there as a local add is. A check's `--target` specs are sent as written |
| `check probe` | Probes from the daemon, so the result is what the daemon's network sees. It writes nothing and takes a `readonly` token |
| `event list` | Lists the daemon's events with the same `--source`, `--outcome` and `--limit` |

The source, secret, run-secret, schedule, rule, silence, mute, check and netgroup writes take
an `operator` token. `check list` and `event explain` stay pending because the API's views carry
no snooze state and no trace, and `check snooze`, `check unsnooze` and the netgroup and check
`apply`, `export`, `show`, `usage` and `test` verbs have no route yet. A secret exocomp makes for
itself, a deploy key, the fleet key or a sealed play or tofu output, cannot be sealed, replaced,
removed or bound over the API, and neither can the `ab:` and `tf:` bindings that collect outputs
([Decision 0027](../decisions/0027-credential-plane-over-the-api.md)). Do that on the daemon's
host with `--local`. A `source rm` followed by an `add` of the same name needs `exocomp --local
secrets rm source/<name>/deploy-key` there first.

Every write accepts `--idempotency-key` (or `EXOCOMP_IDEMPOTENCY_KEY`), which travels as the
`Idempotency-Key` header: a retried request dedups to the one job instead of enqueuing a
second. The daemon namespaces the key by the authenticated token and the route, so a key only
ever dedups *your* retry of *that* verb — a guessable key chosen by one caller cannot swallow
another's write (see the [threat model](../security/threat-model.md)).

`--output json` re-emits the daemon's own envelope — `job.enqueued`, `approval.pending`,
`approval.decision` — byte for byte. A *followed* run emits one document, the settled job,
rather than a stream of per-event envelopes. A followed `command` or `ansible` emits the same
`command.result` document a local run prints, built from the daemon's job and its events, and
a gated one emits the approval. The daemon answers a silence, mute, check or netgroup removal
with no body, so the client prints a small envelope naming what it removed. A remote source,
secret, run-secret, schedule, rule, silence, mute, check or netgroup verb prints the daemon's
envelope under `--output json`, where the local verb prints its text.

## Commands that do not go to the remote

With a remote selected, a command not listed above refuses before it opens any data dir,
instead of writing to a store the daemon never reads; under `--output json` the refusal is a
`wire` error envelope with code `remote_unsupported`. [Decision 0026](../decisions/0026-remote-first-cli.md) sorts every
command into one set:

| Set | With a remote selected | Commands |
|---|---|---|
| Exempt | runs on this machine and ignores the selection | `version`, `help`, `completion`, `commands`, `login`, `hooks validate`, `serve`, `scheduler run`, `web`, `ssh-serve`, `setup`, `upgrade`, `rollback`, `restore`, `sandbox-canary` |
| Local-only | refused; run it on the daemon's host with `--local` | `init`, `identity migrate`, `seal`, `kit derive`, `kit verify`, `recovery-kit`, `backup drill`, `jobs redrive`, `token`, `workspace token`, `workspace backend`, `workspace adopt`, `litestream`, `tui`, `approval link` |
| Pending | refused until its API route exists; `--local` runs it against this machine's data dir | every other command |

The local-only commands need the host's keys, act on what a restart of the host left, or would
carry a credential over the API. The decision record gives the reason for each.

## A machine that only drives a remote daemon

When the daemon runs elsewhere, a container with its own volume or another host, the CLI on
this machine needs no data dir of its own:

```
exocomp login server.tailnet.ts.net:8443 --token exo_...
exocomp doctor
exocomp command --source infra --host web -- uptime
```

After the `login`, `doctor` reports the daemon's health and `command` runs on the daemon from
the `infra` checkout there. A pending or local-only command refuses instead of creating a
local store. On the daemon's own host, an operator whose `hosts.toml` names a default runs a
local-only command with `--local`, for example `exocomp --local token list`.

## What does not cross the wire

A remote write resolves its inputs on the daemon, so an input naming a file on the laptop is
refused rather than sent:

- a playbook path without `--source` (pass `--source <name>`, or an absolute path that exists
  on the daemon)
- `--inventory ./hosts.ini` on `playbook run`, `command` or `ansible` (name a registered
  inventory instead)
- `--privileged` on `playbook run`, `command` or `ansible` (a remote run's sandbox tier is
  the daemon's, set on that host)
- `--extra-var @file` (pass `key=val`, which travels in the request)
- a relative `--ssh-identity` (give the absolute path of a key the daemon's operator allowed
  with `serve --allow-ssh-identity`)
- `workspace apply --plan <path>` (name the saved plan's artifact id)
- `--wait`, which drives a hooked source's composed workflow — a lifecycle the write API does
  not offer

## Following a job

`jobs follow` and a followed `playbook run` consume the daemon's SSE event stream. The stream
is authoritative only from the moment it attaches, so each attach is paired with a read of the
append-only event log from the last sequence; events are deduped by sequence. A dropped
connection is re-established **once**, resuming from that sequence — a stream that keeps
dropping fails the command rather than looping quietly.

## Exit codes

A remote failure surfaces the daemon's `wire` error envelope (preserving its taxonomy `code`,
not just a status) on `--output json`, or the plain error on text, and exits exactly as the
same local failure would (`not_found` → 3, `forbidden` → 4, and the rest of the table). A
certificate that no longer matches the pin is a `PinMismatchError`, refused before any request
is sent, telling the operator to re-run `login` if the re-key is expected. A followed job exits
with the job's own code; a degraded run (tolerated unreachable hosts) is a success.

`workspace plan` is the one deviation: locally it exits 2 when the plan found changes, read
from the run's own driver output. That detail is not in the read API's event projection, so a
remote plan exits 0 once the job is queued; read the outcome from the job's events.

Related: [LAN access](lan-access.md) · [Running the daemon](serve.md) ·
[The API surface](api.md) · [Machine-readable output](machine-output.md) ·
[Playbook run flags](playbook-run.md).
</content>
</invoke>
