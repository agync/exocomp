---
type: Design
title: "Bootstrap-first chain"
description: "The ansible-first-then-tofu handoff for the Kubernetes case, and its credential capture."
status: stable
tags: [design, bootstrap, kubernetes]
---

# Design — Bootstrap-first: the ansible→tofu mirror

## Scope

The ansible-first bootstrap chain: an `ansible-play` step followed by a `tofu-apply`
step, with a credential captured from the play and handed to the apply. It is the mirror
image of the provision-first handoff (items A/D of [v1.1-roadmap.md](v1.1-roadmap.md)),
built for the case where **Ansible bootstraps `kubeadm` nodes, then Terraform manages
cluster resources** through the `kubernetes`/`helm` providers — the reverse of the
tofu-first chains, which run `tofu apply` first and hand its outputs to a play.

The Sequencer runs steps in list order regardless of kind (`Sequencer.Start` accepts any
`[]Step`; `nextStep` walks to the first not-yet-succeeded step,
`internal/queue/workflow.go:94, 186-197`), so the reverse ordering is a property of the
step slice a verb builds, not a runtime capability. Two pieces of plumbing make the
credential handoff work in this direction, each mirroring a tested tofu-side twin.

---

## 1. The tofu-side twins this mirrors

The tofu→ansible handoff rests on two capabilities the ansible-first chain reflects:

- **Item A** — the durable apply→(onboard)→converge chain over the step-ledger Sequencer.
- **Item D** — a `sensitive = true` tofu output sealed after a successful apply and bound
  as a run-secret env for the dependent play. `captureTofuInventory` runs post-apply
  (`internal/executor/tofu.go:276-278`), routing each sensitive output through the
  `OutputSealer` interface (`internal/executor/tofu_output.go:172-174`), whose
  implementation `EnvelopeOutputSealer` seals under the envelope DEK and binds
  `tf:<workspace>`→env (`internal/executor/tofu_seal.go:42-131`), on both the daemon
  (`cmd/exocomp/serve.go:860`) and daemonless (`cmd/exocomp/workspace.go:803`) apply paths.

The default chains put `tofu apply` first: `exocomp handoff run` composes
`[tofu-apply, ansible-play]` (`cmd/exocomp/handoff.go:190-216`), `exocomp provision run`
composes `[tofu-apply, ssh-wait, host-onboard, ansible-play]`
(`cmd/exocomp/provision.go:229-278`), and `exocomp run` is ansible-only. The bootstrap
chain is the composition none of them build: `[ansible-play, tofu-apply]`.

---

## 2. The ansible-output capture → seal hook

The ansible driver captures a declared value from a successful play and seals it for a
following step, mirroring `EnvelopeOutputSealer`/`captureTofuInventory` on the ansible side.

- **Kind.** `SecretKindAnsibleOutput = "ansible-output"`, the ansible twin of
  `SecretKindTofuOutput` (`tofu_seal.go:16`), bound into the sealed row's AAD exactly as
  the tofu kind is — the AAD stays `name|kind` (`tofu_seal.go:83`), a new kind constant
  only — so a moved ciphertext fails to open against a substituted provenance.
- **Capture surface.** After a play succeeds, the driver reads a declared capture target
  and seals it. Two source shapes, both idiomatic to ansible:
  - a **registered fact** the play sets (e.g.
    `exocomp_capture: { KUBECONFIG: "{{ kubeconfig_b64 | b64decode }}" }`), read from a
    facts file the driver points the play at, the way extra-vars are staged into the
    private dir; or
  - a **fetched file** the play `fetch`es into a known path under the run's private dir.

  The driver reads the captured bytes in-process, never through the streaming event sink —
  the discipline `captureTofuInventory` uses because `tofu output -json` prints secrets
  (`tofu_output.go:196-218`).
- **Seal and bind.** An `AnsibleOutputSealer` mirrors `EnvelopeOutputSealer.SealOutput`
  (`tofu_seal.go:62-90`): seal under the envelope DEK, upsert the row by a deterministic
  name (`ansible-output/<workflow-or-play-id>/<name>`), then add a run-secret binding the
  dependent tofu step names ahead of time. The binding is `(env name → secret name)`; the
  value stays in the envelope and is opened per run (`tofu_seal.go:94-131`).
- **Pre-binding.** The producing play and consuming tofu-apply agree on a deterministic
  binding name up front — `ab:<workflow>` (ansible-bootstrap), resolved by the tofu step
  of the same workflow and by nothing else — the mirror of how the handoff publishes
  `tf:<workspace>` before the apply writes it (`handoff.go:184-187`).

The hook captures only what the operator **declares** as a capture target. It never
scrapes stdout, never reads arbitrary facts, and an undeclared sensitive value is never
captured; a missing sealer drops the capture rather than leaking it (mirror of
`tofu_output.go:163-167`).

---

## 3. The `bootstrap` verb

`exocomp bootstrap run` composes `[ansible-play, tofu-apply]` on the Sequencer — the mirror
of `handoff`, argument order reversed:

```
exocomp bootstrap run <playbook> <workspace> \
    [--vault-project P] [--run-secrets NAME] [--source S] [--timeout D] [--wait] \
    [--capture k=fact|@file ...] [--capture-file NAME=FILE ...] \
    [--secret-staging {auto,tmpfs,disk}]
```

Step 1 runs the bootstrap play; its capture hook (§2) seals the credential and binds
`ab:<workflow>`. Step 2 is a `tofu apply` whose `RunSecretBinding` is that same
`ab:<workflow>`, so the sealed credential reaches the `kubernetes`/`helm` provider with
nothing for an operator to copy between steps. Without a capture flag, the play runs and
then a plain apply runs — for the case where the tofu step needs no bootstrap-produced
credential (it reads its own pre-existing run-secret binding). The verb reuses the
`queue.Step`/`queue.Spec` shapes, the `--source` hook-wrap (`handoff.go:257-292`), and the
`--wait` follow (`handoff.go:297-330`) from `handoff.go`, and is a pure addition: the
tofu-first chains are unchanged.

A generic `exocomp workflow` composer — an ordered list of typed steps from a small spec
file (`- tofu-apply <ws>` / `- ansible-play <playbook>` / `- gate …`) straight onto the
Sequencer — is a larger, more general surface that defers the capture-binding convention
to the operator. It is deferred until a third concrete ordering justifies it.

---

## 4. KUBECONFIG delivery: env vs file

The `kubernetes`/`helm` providers authenticate either from decomposed env fields or from a
single `KUBECONFIG` file path. exocomp supports both.

**`--capture` (env decomposition).** Decompose the kubeconfig into the provider's env
fields and bind them as run-secrets: `KUBE_HOST`, `KUBE_TOKEN` (or the
`KUBE_CLIENT_CERT_DATA` / `KUBE_CLIENT_KEY_DATA` / `KUBE_CLUSTER_CA_CERT_DATA` triple),
`KUBE_INSECURE`, and so on. `runSecretEnv` renders these as `KEY=VALUE` env entries
(`runsecret.go:28-50`) and the providers read them natively. No secret is written to a
filesystem. The capture hook seals several named fields, and an operator who thinks in
whole kubeconfigs must know the mapping.

**`--capture-file NAME=FILE` (file staging).** Deliver a captured value to the apply as a
file path: the daemon writes the sealed bytes to a `0600` file and exports `NAME=<that
path>` for the tofu step. This is the copy-paste form and the one exec-auth tools that
read a file need; it needs no field decomposition. The value never reaches an argv, event,
or the ledger, and the file is unlinked once the subprocess that read it exits. The tofu
driver gains a secret-file staging step (`internal/executor/runsecret_file.go`:
`StageRunSecretFiles`, `StagedRunSecretFile.Close`, `SweepOrphanRunSecretFiles`;
`TofuOptions.RunSecretFiles` wired in `tofu.go` and `sandboxedTofu.Run`, which grants the
staged path via `SandboxProfile.ReadFiles` so the sandboxed tier can read it).

| | `--capture` (env) | `--capture-file` (file) |
|---|---|---|
| Secret on disk | never | 0600 staged file, one run |
| Code | capture hook seals N fields | capture hook + tofu-side file staging + `KUBECONFIG` export |
| Operator ergonomics | must map fields | paste a kubeconfig, get a path |
| Exposure surface | env-only | a staged file holding a bearer |

Env decomposition is the lower-exposure default; file staging is for the copy-paste
`KUBECONFIG` case and exec-auth tools, with its exposure bounded by the staging design below.

---

## 5. Run-secret file staging

A staged file holds a plaintext bearer, so its backing store is a run-secret's one point
of filesystem exposure. Staging targets a per-instance session directory
`<root>/exocomp.<pid>.<rand>/`, with 0600 files inside a 0700 directory, and the root is
selected by `--secret-staging {auto,tmpfs,disk}`:

- **`auto`** (default) stages onto `/dev/shm` when it is usable — present, backed by tmpfs,
  adequate free space — and falls back to the disk root (`<data-dir>/run/`, the tree job
  private directories already use) otherwise, with a warning on the daemon's log and a job
  event on every run that lands a secret there.
- **`tmpfs`** forces `/dev/shm` and fails closed if it is unusable — the strict "the staged
  secret never touches durable disk" posture an operator can demand by name. `/dev/shm` is
  RAM-backed unconditionally in every rootless podman container, needs no mount, quadlet
  entry, or capability, and has a real path the sandboxed tier can grant (unlike a memfd,
  which has no path a sandboxed child's `--read-file` grant can name, since `nono` closes
  inherited fds before exec).
- **`disk`** forces the disk root, for a host with no usable tmpfs.

The staged bytes are mlocked best-effort — no rootless swap-avoidance option exists for
tmpfs itself, so mlock is the only lever; a host that forbids it still stages, unlocked,
with the residual reported by `exocomp doctor`. The mmap-based lock is filesystem-agnostic,
so it covers both backends. `<data-dir>/run/` sits outside every backup's captured set:
`internal/backup`'s staging tree is built by writing four named files, never by walking the
data dir (see [log.md](../log.md)).

**Cleanup** is three layers: a per-job unlink on `Close`; a `RemoveAll` of the whole
session directory on a graceful stop; and a liveness-checked startup sweep that parses each
session directory's owning pid out of its name and removes only the ones whose process is
no longer running (`pidAlive`, mirroring `store.ownerAlive`'s existence-and-permission
probe without importing `store`). A directory owned by a live pid is never touched.

**Residuals.** On `disk`, a hard crash (a panic between staging and `Close`, a `kill -9`)
leaves the 0600 session directory on durable storage until the next startup sweep reaps it;
a daemon that never restarts leaves it indefinitely. On `tmpfs` the same crash can leave a
directory, but a fresh container start clears `/dev/shm`, so the residual cannot outlive the
container. The sweep tells a live owner from a dead one by pid, and a container restart's
small pid space means a fresh process can in principle reuse a dead pid before the sweep
runs, reading a stale directory as alive and skipping it — bounded (nothing security-relevant
depends on it as a fail-open primitive) and documented at `internal/executor/pidalive.go`.

---

## 6. Security notes

- **Values-free everywhere.** The ansible sealer never logs, events, or otherwise surfaces
  the captured value, as the `OutputSealer` contract requires of sensitive tofu outputs
  (`tofu_output.go:159-174`); the capture summary carries names and counts, never values
  (`InventoryCapture`, `tofu_output.go:44-56`). Captured bytes are read in-process, never
  through the streaming event sink.
- **Reuse the redaction path.** The ansible run builds a `secrets.NewRedactor` over the
  run's sealed material and vault-derived bytes (`cmd/exocomp/ansible.go:104-108`); a
  captured value joins that material set, so any incidental echo in a later step's stream is
  scrubbed by the existing redactor.
- **AAD unchanged.** The sealed row's AAD stays `name|kind` (`tofu_seal.go:83`); the only
  change is the `ansible-output` kind constant, so a ciphertext still fails to open against a
  moved name or substituted kind.
- **Default-deny.** The capture seals only operator-declared targets. Under `--capture` no
  secret touches a filesystem; under `--capture-file` the staged file is bounded by the
  staging design (§5) and unlinked once the reading subprocess exits.
