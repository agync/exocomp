# Driving a remote daemon from a laptop

`--remote` points the CLI at another box's `/api/v1` instead of a local data dir. Reads
render the daemon's own projections; writes enqueue there and the job can be watched live.
The transport is the ADR-0009 TLS listener ([LAN access](lan-access.md)) with the
certificate pinned trust-on-first-use.

## Pinning a remote

```
exocomp login server.tailnet.ts.net:8443 --token exo_...
exocomp --remote server jobs list
```

`login` pins the presented certificate's SHA-256 fingerprint and stores
`{endpoint, fingerprint, token}` in `~/.config/exocomp/hosts.toml` (0600). Later calls
verify the pin and refuse to connect if it changed.

Accepting a **changed** fingerprint (or re-pointing an alias at a different endpoint) takes
`--replace-pin`, and nothing else does it: `--assume-yes` pins a *first* fingerprint on
non-interactive stdin but never replaces one. That split is deliberate — every script, CI
step and automated caller must pass `--assume-yes` to pin at all, so if it also replaced pins,
the one invocation automation always uses would re-pin to whatever certificate answered and
hand it the bearer token (see the [threat model](../security/threat-model.md)).
Confirm the new fingerprint out of band first; a refusal exits non-zero and leaves
`hosts.toml` untouched.

A command with neither `--remote` nor `EXOCOMP_REMOTE` stays **local**, even when a default
remote is stored: a stored default fills a valueless `--remote=`, it never redirects a bare
command off the box.

## Writes

| Command | Over `--remote` |
|---|---|
| `playbook run` | Enqueues, then follows the job to its end and exits with the job's code. `--no-follow` prints the id and exits 0 |
| `workspace plan` | Enqueues, prints the job id |
| `workspace apply` | Enqueues, prints the job id — or the pending approval, its policy, deadline and TTL when the plan's verdict gates it |
| `approval decide` | Settles the approval on the daemon, which derives the capability token itself. Needs an **`approve`-scope** token: `operator` may enqueue but may not settle what it enqueued, and no token may decide an approval for a job it asked for itself |
| `jobs follow <id>` | Streams the job's events and exits with the job's code |

Every write accepts `--idempotency-key` (or `EXOCOMP_IDEMPOTENCY_KEY`), which travels as
the `Idempotency-Key` header: a retried request dedups to the one job instead of enqueuing
a second. The daemon namespaces the key by the authenticated token and the route, so a key
only ever dedups *your* retry of *that* verb — a guessable key chosen by one caller cannot
swallow another's write (see the [threat model](../security/threat-model.md)).

`--output json` re-emits the daemon's own envelope — `job.enqueued`, `approval.pending`,
`approval.decision` — byte for byte. A *followed* run emits one document, the settled job,
rather than a stream of per-event envelopes.

## What does not cross the wire

A remote write resolves its inputs on the daemon, so an input naming a file on the laptop
is refused rather than sent:

- a playbook path without `--source` (pass `--source <name>`, or an absolute path that
  exists on the daemon)
- `--inventory ./hosts.ini` (name a registered inventory instead)
- `--extra-var @file` (pass `key=val`, which travels in the request)
- `workspace apply --plan <path>` (name the saved plan's artifact id)
- `--wait`, which drives a hooked source's composed workflow — a lifecycle the write API
  does not offer

## Following a job

`jobs follow` and a followed `playbook run` consume the daemon's SSE event stream. The
stream is authoritative only from the moment it attaches, so each attach is paired with a
read of the append-only event log from the last sequence; events are deduped by sequence.
A dropped connection is re-established **once**, resuming from that sequence — a stream
that keeps dropping fails the command rather than looping quietly.

## Exit codes

A remote failure carries the daemon's taxonomy code and exits exactly as the same local
failure would (`not_found` → 3, `forbidden` → 4, and the rest of the table). A followed job
exits with the job's own code; a degraded run (tolerated unreachable hosts) is a success.

`workspace plan` is the one deviation: locally it exits 2 when the plan found changes,
which is read from the run's own driver output. That detail is not in the read API's event
projection, so a remote plan exits 0 once the job is queued; read the outcome from the
job's events.

Related: [LAN access](lan-access.md) · [Running the daemon](serve.md) ·
[Playbook run flags](playbook-run.md).
