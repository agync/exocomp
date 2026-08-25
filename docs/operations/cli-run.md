# Governed CLI runs

Some operations are neither ansible nor tofu: a `kubectl rollout`, a scoped token refresh,
a vendor deploy CLI. Until now they reached the fleet through an `ansible shell` wrapper,
which is a way of *avoiding* this control plane's guarantees rather than using them — the
queue sees an ansible job, the argv on the row is a shell string, and nothing decides which
programs may run.

`exocomp run-cli` is the run type for that work. It is an ordinary job of kind `cli`: the
one queue, the nono sandbox, a per-job private directory, run-secret injection, redaction,
the approval gate, and exit-code-is-truth — the same machinery a playbook or a `tofu apply`
gets.

## The allow-list is the safety property

A CLI run can only execute a binary an operator registered:

```
exocomp cli-tool add kubectl --binary kubectl \
  --fixed-arg --context=homelab \
  --arg-pattern '[a-z0-9./=-]+' \
  --egress https --assume-yes
```

A name with no registration is refused **before anything is enqueued** — no job row, no
worker, nothing to approve later. `run-cli` is therefore not "run any program under the
sandbox": it is "run one of the allow-listed programs, under the sandbox".

Registering widens what this control plane can execute, so `cli-tool add` takes the
destructive-confirm contract (`--assume-yes`, or a clear refusal on a non-interactive
session). `cli-tool rm` does not: removing an entry only narrows the boundary.

Entries live in the database, beside sources, inventories, runners and plan policies. The
boundary deciding what may execute is then restored by the same backup that restores the
rest of the control plane, rather than sitting in a config tree the data volume does not
carry.

| Registration flag | What it decides |
|---|---|
| `--binary` | The executable: a name resolved on PATH, or an absolute path. One token — it is exec'd directly, and a value carrying whitespace is refused |
| `--fixed-arg` (repeatable) | Arguments prepended to *every* invocation, so the part that must not vary (`--context prod`) stops depending on each caller repeating it |
| `--arg-pattern` | An anchored regexp every operator-supplied argument must match, narrowing a broad tool to the shape it is registered for |
| `--egress` | `none` (default) reaches no network; `https` opens port 443 on that run and nothing else |

## Running one

```
exocomp run-cli kubectl -- rollout restart deploy/web
```

Everything after `--` is the tool's own argv, and it stays a list. There is no shell
anywhere on the path: a `;`, a `|`, a `*` or a `$(...)` in an argument arrives at the
process as one literal argument and means nothing to anything. That is a structural
property, not a filter — the argv is assembled as a slice, recorded on the job row as a
slice, and handed to `execve` as a slice.

| Flag | Behaviour |
|---|---|
| `--source` | Run from that registered source's checkout; it is the only repository path the run may reach |
| `--run-secrets` | Bind a `run_secret` set as named environment for this run alone |
| `--require-approval` | The job lands in `awaiting_approval`, where no worker can claim it, until a decision |
| `--concurrency-key` | Serialize against other jobs holding the key (default `cli:<tool>`; pass `tofu:<workspace>` to serialize with that workspace's runs) |
| `--idempotency-key` | A retried run with the same key resolves to the job the first call created |
| `--output json` | The `job.get` wire envelope is the only thing on stdout |

The process exits with the job's own exit code. The job succeeds when the tool exits 0 and
fails otherwise; nothing parses the tool's output to second-guess it (AGENTS.md invariant
3), which is exactly why a vendor CLI is safe to run this way without a per-tool adapter.

## Credentials

A provider token reaches the tool as an environment variable and never as an argument:

```
exocomp secrets add cloudflare-token          # value on stdin
exocomp run-secret add cf CLOUDFLARE_API_TOKEN cloudflare-token
exocomp run-cli wrangler --run-secrets cf -- deploy
```

The job row carries the binding *name*; the sealed value is opened from the envelope when
the job runs, injected into the child's environment, and added to what the run's redactor
scrubs from events. It never appears on argv (which the job row records and the process
table exposes) and never in a job event.

## What the sandbox grants

Under the `enforcing` tier the profile is default-deny, like every other kind, and a
CLI run may touch:

- its own per-job private, work, and temp directories (`TMPDIR` is redirected into them);
- the tool tree the registered binary resolves to, read-only;
- the source checkout, when `--source` names one — it is the run's working directory;
- port 443, and only when the registration declared `--egress https`.

Egress is per *registration*, not per kind. `ansible` and `tofu` each speak one protocol, so
a port belongs to their kind; the `cli` kind is every allow-listed binary at once, and a
kind-wide grant would hand each of them the widest posture any single one asked for.

`HOME` is redirected to an empty per-run directory that is created and deliberately **not**
granted. A tool that insists on reading its own dotfile configuration will not find one:
point it at a file in the source checkout, or bind its configuration through a run-secret
environment variable instead.

## Running against a daemon

With `exocomp serve` up, `run-cli` enqueues and follows the job the daemon runs, the same
shape `workspace apply` and `playbook run` already have — it does not drain a queue of its
own. The daemon rebuilds the driver per job from the allow-list entry the row's argv[0]
names; an entry removed between enqueue and claim fails the job closed rather than running
whatever that name resolves to on the daemon's PATH.

## Not yet

The web/MCP surfaces for the `cli` kind are follow-ups.
