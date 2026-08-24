# Ansible-first bootstrap (`bootstrap run`)

`exocomp bootstrap run <playbook> <workspace>` composes the mirror of the
[Terraform→Ansible handoff](terraform-ansible-handoff.md): the ansible **play
runs first**, then `tofu apply` runs against the workspace, as one step-ledger
workflow. It is the ordering for a persona where Ansible bootstraps nodes and
Terraform then manages resources on top of them — `kubeadm` brings up a control
plane, then the `kubernetes`/`helm` providers manage cluster objects.

This is not a new engine. The [Sequencer](../adr/0008-step-ledger-over-dbos.md) has
always run steps in list order; every shipped verb happened to put `tofu apply`
first. `bootstrap run` is the CLI verb that composes `[ansible-play, tofu-apply]`
instead. The apply runs only if the play succeeded; a daemon that dies mid-chain
resumes it from the ledger, exactly as the handoff does.

## The chain

```
exocomp bootstrap run site.yml wsp --inventory hosts --wait
```

Two steps, in order:

1. **ansible-play** — the bootstrap play, run against `--inventory`. Because the
   apply has not run yet, there is no materialized `tf:<workspace>` inventory to
   build the play's hosts from, so the play targets an inventory you supply
   (`--inventory <path|name>`, or localhost by default).
2. **tofu-apply** — `tofu apply -auto-approve` against the workspace, gated on the
   workspace's tofu concurrency key like every other apply.

A running `exocomp serve` drives the chain: it releases step 1, and step 2 only
once step 1 reached succeeded. Without a daemon the command refuses rather than
leaving a chain that will never progress.

## Two ways the apply gets its credentials

**Without `--capture` (the standing-credential case).** The two steps run in order
and nothing is captured between them. The apply reads its own pre-existing
credential — bind it with `--run-secrets <name>` exactly as the handoff binds one,
e.g. a `KUBECONFIG`-shaped run-secret the operator sealed ahead of time, or the
apply's own `~/.kube/config` via a standing binding.

**With `--capture` (the play produces the credential).** The bootstrap play fetches
a sensitive value off the nodes it just brought up — the `kubeadm` kubeconfig, an
`admin.conf` off a control-plane node — and the chain seals it and hands it to the
apply as environment, with nothing for you to copy. See the next section.

## `--capture`: seal a play output into the apply's env

```
exocomp bootstrap run site.yml wsp \
    --capture KUBE_HOST=kube_host --capture KUBE_TOKEN=kube_token --wait
```

Each `--capture NAME=FILE` declares one output to hand from the play to the apply:

- **`FILE`** is a path, relative to the run's capture directory, the play `fetch`es
  the value into. The driver exports that directory to the play as the
  `exocomp_capture_dir` variable; the play writes `{{ exocomp_capture_dir }}/<FILE>`.
- **`NAME`** becomes the environment variable the apply receives, upper-cased
  (`kube_token` → `KUBE_TOKEN`).

After the play succeeds, the daemon reads each declared file **in-process** — never
through the event stream — seals it under the run's envelope, and binds it under
`ab:<workflow>`. The apply step of the same chain resolves that binding and receives
the sealed values as env. No value is written to a filesystem the way a `KUBECONFIG`
file would be: this is the **env-decomposition** path, so the `kubernetes`/`helm`
providers authenticate from their native `KUBE_HOST` / `KUBE_TOKEN` /
`KUBE_CLIENT_CERT_DATA` … variables. Capture whichever fields your providers read.

Rules that keep it safe and predictable:

- **Only what you declare is captured.** An undeclared fact is never scraped; a
  declared file the play did not produce is reported, not fatal (the play already
  changed the hosts — its exit code is the verdict).
- **A name that is not a POSIX identifier is skipped, not mangled.** `--capture
  kube-config=…` folds to `KUBE-CONFIG`, which is not a valid env name; the value is
  sealed but left unbound with a reason in the play's events, never rewritten into
  some `KUBE_CONFIG` the operator never asked for.
- **Values never surface.** The sealed value is absent from every argv, job event,
  and the ledger; only the binding name and the env name travel. A re-run reseals
  in place.
- **`--capture` and `--run-secrets` are mutually exclusive.** The apply has one
  run-secret binding slot; `--capture` claims it for the chain's own
  `ab:<workflow>`. Bind pre-existing credentials this way only when the play does
  not produce them.
- **A crash mid-play does not lose the captures.** The declarations and the
  workflow id ride the job row as a first-class field, so a play the daemon
  re-arms after a mid-run crash resumes with them intact — it seals under the same
  `ab:<workflow>` the interrupted run would have, and the apply's binding is never
  left empty by the resume.

For a tool that reads a whole `KUBECONFIG` **file** rather than decomposed env
fields (`kubectl`, `helm`, an exec-auth provider), use `--capture-file` — the next
section.

## `--capture-file`: hand the apply a KUBECONFIG file

```
exocomp bootstrap run site.yml wsp \
    --capture-file KUBECONFIG=kubeconfig --wait
```

`--capture-file NAME=FILE` seals a play-fetched file exactly as `--capture` does,
but the apply receives it as a **file path** rather than an env value: the daemon
writes the sealed bytes to a `0600` file under `/dev/shm` and sets `NAME` to that
path (`KUBECONFIG=/dev/shm/exocomp-secret-…`). A tool that wants a config file
gets one, with no field decomposition to map.

The value only ever exists in that file — never in an argv, an event, or the
ledger — and the file is unlinked once the apply's subprocess exits.

**The staged file lives in RAM, unconditionally.** `/dev/shm` is present in every
rootless podman container with no mount, no quadlet entry, and no capability
needed — there is nothing here to fail closed on, unlike the tmpfs run-directory
mount this replaced. Its bytes are additionally mlocked best-effort (there is no
rootless swap-avoidance option for tmpfs itself); `exocomp doctor` reports the
mlock posture under **run-secret file staging**, and a host that cannot mlock
still stages, unlocked, with the residual reported rather than the apply refused.
A startup sweep removes any orphaned staged file a broken cleanup path left
behind. Env-decomposition (`--capture`) does not stage a file at all.

`--capture-file` shares every safety rule of `--capture` above (declare-only
capture, POSIX-name skipping, values-free, re-run idempotence) and the same single
binding slot, so it is likewise mutually exclusive with `--run-secrets`. The two
capture forms may be mixed: `--capture KUBE_HOST=… --capture-file KUBECONFIG=…`
delivers some values as env and others as a file in one chain.

## Flags

| Flag | Effect |
|---|---|
| `--inventory <path\|name>` | Inventory for the bootstrap play (default: localhost). |
| `--vault-project <p>` | Derive the play's ansible-vault password from the envelope. |
| `--run-secrets <name>` | Bind a run-secret set as provider/`TF_VAR_` env for the **apply** step. Mutually exclusive with `--capture`/`--capture-file`. |
| `--capture NAME=FILE` | Seal a play-fetched file (`FILE`, relative to the run's capture dir) and hand it to the **apply** as `NAME` env. Repeatable. Mutually exclusive with `--run-secrets`. |
| `--capture-file NAME=FILE` | Like `--capture`, but deliver the sealed value as a **file**: `NAME`=path to a `0600` file under `/dev/shm` for a tool that wants a `KUBECONFIG` file. Repeatable. Mutually exclusive with `--run-secrets`. |
| `--source <s>` | Wrap the chain in a registered source's `.exocomp/hooks.yml` lifecycle — pre-run hooks before the play, post-run hooks and the health gate after the apply — identically to `handoff run --source`. |
| `--require-approval` | Gate the destructive apply behind a durable operator approval the daemon honors mid-chain: the apply step waits in `awaiting_approval` until `exocomp approval decide` settles it. |
| `--timeout <d>` | Per-step timeout (0: no limit). |
| `--wait` | Follow the workflow to a terminal state. |
| `--output json` | Emit the wire envelope for the started workflow instead of prose. |

`exocomp bootstrap status <workflow-id>` prints the chain's steps from the ledger.

### Gating the destructive apply

The chain ends in a destructive `tofu apply`. `--require-approval` gates that
step behind the **same durable approval a single destructive apply passes
through**: the Sequencer does not release the apply on its own. When the chain
reaches it, the daemon arms a pending approval (minting the capability and
paging the operator exactly as the reactive gate does) and leaves the job in
`awaiting_approval`. The play (and any earlier step) has already run; nothing
after the gated step runs until a decision settles it.

Settle it with the same seam every approval uses — `exocomp approval decide <id>
--approve` / `--reject`, the phone/browser decision link, or an approve-scope
`/api/v1` token:

- **Approve** releases exactly that step into the queue; the running daemon
  claims and applies it, and the chain continues.
- **Reject** cancels the apply and blocks every step after it; the workflow ends
  failed with the downstream steps `blocked` in the ledger.

The gate is durable, not a CLI confirmation: it holds across a daemon restart
(the requirement is a ledger property, re-armed when the step becomes current),
and the claim path itself refuses a job whose approval is still pending, so a
running daemon cannot run the apply until the operator answers. The approval's
TTL starts when the step becomes current — a slow play cannot burn the window
before the apply is even reached.

## Machine output

```
exocomp bootstrap run site.yml wsp --output json
```

emits a success envelope whose `data` carries the workflow id and the ordered
step kinds, so a caller can confirm the chain is ansible-first without parsing
prose:

```json
{"schema_version":1,"kind":"bootstrap.workflow","data":{"workflow_id":"bootstrap:wsp:...","workspace":"wsp","playbook":"/abs/site.yml","steps":["ansible-play","tofu-apply"]}}
```

## Not to be confused with

The [Bootstrap](bootstrap.md) operations page covers first-run install and the
setup wizard — a different sense of the word. This page is the `bootstrap run`
verb: an ansible-first execution chain.
