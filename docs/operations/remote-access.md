---
type: Guide
title: "Driving a remote daemon"
description: "Login and the remote flag: certificate pinning, precedence, pinned reads, writes that enqueue on the other box, following a job live, and the inputs that do not cross the wire."
status: stable
tags: [operations, remote, cli]
---

# Driving a remote daemon

`exocomp login` pins a remote daemon's certificate and a bearer token so the same CLI can
drive a box other than the one it runs on. `--remote` then points any command at that box's
`/api/v1` instead of the local data dir. Reads render the daemon's own projections; writes
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

Every command inherits two global flags plus the token flag:

| Input | Precedence |
|---|---|
| `--remote <selector>` | wins over the environment |
| `EXOCOMP_REMOTE` | used when `--remote` is not passed |
| neither | the command stays **local**, against the data dir, unchanged |
| `--remote=` (empty value) | remote mode is active; resolves to the `hosts.toml` default |

`<selector>` is either a stored alias (`server`) or a raw `https://` endpoint pinned under
some alias by an earlier `login` — an endpoint that was never logged in is refused (TOFU
demands the pin exist first). The `hosts.toml` default is consulted **only** once remote mode
is already active (via `--remote`/`EXOCOMP_REMOTE`); a bare command with neither never gets
silently redirected off the box, even when a default remote is stored.

A bare `playbook run`, or `workspace plan`, `apply` or `destroy`, runs the job on the machine
the command runs on. When a default remote is stored it says so on stderr first:

```text
running on this machine, not on the default remote "server" (pass --remote= to run there)
```

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

Every write accepts `--idempotency-key` (or `EXOCOMP_IDEMPOTENCY_KEY`), which travels as the
`Idempotency-Key` header: a retried request dedups to the one job instead of enqueuing a
second. The daemon namespaces the key by the authenticated token and the route, so a key only
ever dedups *your* retry of *that* verb — a guessable key chosen by one caller cannot swallow
another's write (see the [threat model](../security/threat-model.md)).

`--output json` re-emits the daemon's own envelope — `job.enqueued`, `approval.pending`,
`approval.decision` — byte for byte. A *followed* run emits one document, the settled job,
rather than a stream of per-event envelopes. A followed `command` or `ansible` emits the same
`command.result` document a local run prints, built from the daemon's job and its events, and
a gated one emits the approval.

Every command not listed above runs against the local data dir and has no remote path:
`source add`/`rm`/`sync`/`egress`, `inventory`, `run-secret`, `schedule`, `rule`, `secrets`,
`token`, `runner`, `backup`, and the rest. With `--remote` or `EXOCOMP_REMOTE` set, such a
command refuses with an error naming the missing support instead of writing to the local
store; under `--output json` the refusal is a `wire` error envelope with code
`remote_unsupported`. Unset both to run it on
this machine. `version`, `commands`, `login`, `help`, `completion`, `hooks validate`, `serve`,
`ssh-serve`, `setup` and `upgrade` run on this machine by nature and ignore the selection.

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
