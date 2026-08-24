# Design — Bootstrap-first: the ansible→tofu mirror

## Status

Proposed · 2026-08-23; Phases 0–3 landed. It specifies the mirror image of the
provision-first handoff (items A/D of [v1.1-roadmap.md](v1.1-roadmap.md)) that the
Kubernetes persona interview surfaced. The shape was agreed here before it was built;
Phases 0 (ansible-first chain), 1 (capture→seal), 2 (env-decomposition binding) and 3
(`--capture-file` file staging) have since shipped — see §3 for what each touched.

The persona: **Ansible bootstraps `kubeadm` nodes, then Terraform manages cluster
resources** through the `kubernetes`/`helm` providers. That is the reverse of every
chain exocomp exposes today, which runs `tofu apply` first and hands its outputs to a
play. The Sequencer already runs steps in any order; the gap is entirely in what the
CLI exposes and in two missing pieces of plumbing, all of which have a tested twin on
the tofu side to mirror.

---

## 1. Problem: exocomp is provision-first only

### 1.1 What exists (provision-first)

Every composed chain today puts `tofu apply` first and an ansible play second:

- `exocomp handoff run <ws> <playbook>` — steps `[tofu-apply, ansible-play]`
  (`cmd/exocomp/handoff.go:190-216`).
- `exocomp provision run <ws> <playbook>` — steps
  `[tofu-apply, ssh-wait, host-onboard, ansible-play]` (`cmd/exocomp/provision.go:229-278`).
- `exocomp run <source> <playbook>` — ansible-only: pre-hooks → play → post-hooks →
  gate, with no tofu step at all (`cmd/exocomp/run.go:207-251`).

The tofu→ansible handoff is backed by two tested capabilities:

- **Item A** — the durable apply→(onboard)→converge chain over the step-ledger Sequencer.
- **Item D** — a `sensitive = true` tofu output sealed after a successful apply and
  bound as a run-secret env for the dependent play: `captureTofuInventory` runs
  post-apply (`internal/executor/tofu.go:276-278`), routing each sensitive output
  through the `OutputSealer` interface (`internal/executor/tofu_output.go:172-174`),
  whose one implementation `EnvelopeOutputSealer` seals under the envelope DEK and
  binds `tf:<workspace>`→env (`internal/executor/tofu_seal.go:42-131`). It is wired on
  both apply paths — daemon (`cmd/exocomp/serve.go:860`) and daemonless
  (`cmd/exocomp/workspace.go:803`).

### 1.2 The three confirmed gaps

**(a) No ansible-output→seal capture.** The tofu driver has a post-apply capture hook
(`tofu.go:276-278`) that materializes an inventory and seals sensitive outputs. The
ansible driver has no equivalent. `Ansible.Run` wraps its stream in observers —
`hostKeyObserver`, `noHostsObserver`, `recapSink` (`internal/executor/ansible.go:231-244`)
— but none of them capture an ansible fact or a fetched file into the secret store.
There is no `SealOutput` twin for ansible, so a play that produces a kubeconfig (the
`kubeadm init` output, or an `admin.conf` fetched from a control-plane node) has nowhere
to put it that a following tofu step can read.

**(b) No ansible-first-then-tofu chain is exposed, though the Sequencer supports it.**
`Sequencer.Start` accepts any `[]Step` and runs them in list order — `nextStep` simply
walks to the first not-yet-succeeded step (`internal/queue/workflow.go:94, 186-197`),
with no compiled-in knowledge of tofu-before-ansible. The ordering is entirely a
property of the step slices the CLI verbs build, and all three of those hard-code tofu
first (§1.1) or omit tofu entirely. Nothing composes `[ansible-play, tofu-apply]`. The
runtime is ready; the exposure is missing.

**(c) Run-secrets are env-only; the kubernetes provider wants a `KUBECONFIG` file.**
`runSecretEnv` renders a `RunSecrets` map to sorted `KEY=VALUE` env entries and nothing
else (`internal/executor/runsecret.go:21-50`). There is no path that writes a sealed
secret to a file. The `kubernetes` and `helm` providers can authenticate from
env (`KUBE_HOST`, `KUBE_TOKEN`, `KUBE_CLIENT_CERT_DATA`, …), which the current binding
already delivers — but the common, copy-paste configuration is a single
`KUBECONFIG=/path/to/config`, and exocomp cannot produce that path today. The only place
a run already stages 0600 secret material to disk is the ansible per-job private dir
(`ansible.go:184-199, 390-465`, the vault-password and extra-vars files); the tofu run
has no such staging.

---

## 2. Design — the mirror of the tested features

Each piece below is specified as the reflection of a shipped, tested tofu-side feature,
so it inherits the same doctrine (exit-code truth, values-free events, deterministic
names, default-deny) rather than inventing a second model.

### 2.1 (a) An ansible fact/fetched-file capture → seal hook

Mirror `EnvelopeOutputSealer` and `captureTofuInventory` on the ansible side.

- **New kind.** `SecretKindAnsibleOutput = "ansible-output"`, the ansible twin of
  `SecretKindTofuOutput` (`tofu_seal.go:16`). It is bound into the sealed row's AAD
  exactly as the tofu kind is (`tofu_seal.go:83`, `<name>|<kind>`), so the provenance is
  part of what a moved ciphertext fails to open against. **AAD construction is unchanged**
  — same `name|kind` shape, a new kind constant only.
- **Capture surface.** After a play succeeds, read a declared capture target and seal
  it. Two source shapes, both already idiomatic to ansible:
  - a **registered fact** the play sets (e.g. `exocomp_capture: { KUBECONFIG: "{{ kubeconfig_b64 | b64decode }}" }`), read from a facts file the driver points the play at, the way extra-vars are staged into the private dir today; or
  - a **fetched file** the play `fetch`es into a known path under the run's private dir.
  The driver reads the captured bytes in-process — never through the streaming event
  sink — the same discipline `captureTofuInventory` uses because `tofu output -json`
  prints secrets (`tofu_output.go:196-218`).
- **Seal + bind.** An `AnsibleOutputSealer` mirrors `EnvelopeOutputSealer.SealOutput`
  (`tofu_seal.go:62-90`): seal under the envelope DEK, upsert the row by a deterministic
  name (`ansible-output/<workflow-or-play-id>/<name>`), then add a run-secret binding the
  dependent tofu step names ahead of time. The binding is `(env name → secret name)`
  only; the value stays in the envelope and is opened per run (`tofu_seal.go:94-131`).
- **Pre-binding for the dependent tofu step.** The producing play and the consuming
  tofu-apply agree on a deterministic binding name up front — the mirror of how the
  handoff publishes `tf:<workspace>` before the apply writes it (`handoff.go:184-187`).
  A candidate: `ab:<workflow>` (ansible-bootstrap), resolved by the tofu step of the same
  workflow and by nothing else.

Non-goals for this hook: it captures only what the operator **declares** as a capture
target. It never scrapes stdout, never reads arbitrary facts, and a sensitive value that
is not declared is not captured — the fail-safe direction.

### 2.2 (b) An ordered-workflow composer / ansible-first handoff verb

The Sequencer is order-agnostic (`workflow.go:94, 186-197`), so this is a CLI-shape
decision, not a runtime change. Two options, not mutually exclusive:

**Option B1 — a focused `exocomp bootstrap` verb (recommended first).** The mirror of
`handoff`, argument order reversed:

```
exocomp bootstrap run <playbook> <workspace> \
    [--vault-project P] [--run-secrets NAME] [--source S] [--timeout D] [--wait]
```

Phase 0 (§3) ships this shape with no capture flag; Phase 2 adds
`--capture k=fact|@file ...` once the capture hook (§2.1) exists.

Steps `[ansible-play, tofu-apply]`. Step 1 runs the bootstrap play; its capture hook
(§2.1) seals the kubeconfig and binds `ab:<workflow>`. Step 2 is a `tofu apply` whose
`RunSecretBinding` is that same `ab:<workflow>`, so the sealed kubeconfig reaches the
`kubernetes`/`helm` provider as env with nothing for an operator to copy. This reuses the
exact `queue.Step`/`queue.Spec` shapes `handoff.go` already builds and the same
`--source` hook-wrap (`handoff.go:257-292`) and `--wait` follow (`handoff.go:297-330`).

**Option B2 — a generic `exocomp workflow` composer.** An ordered list of typed steps
from a small spec file (`- tofu-apply <ws>` / `- ansible-play <playbook>` / `- gate …`),
composed straight onto the Sequencer. More general, larger surface, and it defers the
capture-binding convention to the operator. Reasonable as a **later** generalization once
B1's binding convention is proven; not the first cut.

Both are pure additions: no existing verb changes, and the tofu-first chains keep their
behavior.

### 2.3 (c) OWNER DECISION — KUBECONFIG file vs env-decomposition

This was a genuine fork with a real security tradeoff. **Both are now shipped:** C-env
first (Phase 2), then C-file (Phase 3) once the owner chose it. The owner decided C-file
**is worth building, on the hard condition that the staged secret never touches durable
disk** — the value is staged as a 0600 file under `/dev/shm`, the RAM-backed tmpfs present
unconditionally in every rootless podman container, never under the per-job run directory
on the durable data volume (agentfs/CoW was evaluated for the backing store and rejected in
favour of plain tmpfs; a `noswap` `Tmpfs=` quadlet mount on the run directory was the
original design and was replaced — see "2026-08-24 (later): /dev/shm replaces the tmpfs
run-dir mount" below — because that mount does not start under rootless podman at all).
The fork below is kept for the rationale.

**Option C-env (works today, zero new exposure).** Decompose the kubeconfig into the
provider's env fields and bind them as run-secrets: `KUBE_HOST`, `KUBE_TOKEN` (or the
`KUBE_CLIENT_CERT_DATA` / `KUBE_CLIENT_KEY_DATA` / `KUBE_CLUSTER_CA_CERT_DATA` triple),
`KUBE_INSECURE`, etc. `runSecretEnv` already delivers exactly this shape
(`runsecret.go:28-50`), and the `kubernetes`/`helm` providers read these names natively.
No secret is ever written to a filesystem. Cost: the capture hook (§2.1) must seal several
named fields, and an operator who thinks in whole kubeconfigs has to know the mapping.

**Option C-file (option #4: stage a KUBECONFIG file).** Materialize the sealed kubeconfig
into the run's 0700 per-job private dir (0600, removed on process exit — the same
lifecycle as the vault-password file, `ansible.go:184-199, 427-431`) and export
`KUBECONFIG=<that path>` for the tofu step. This is the copy-paste-friendly form and needs
no field decomposition. Cost: **it is a new exposure surface.** A plaintext bearer written
to a file, even in an ephemeral private dir on tmpfs for the lifetime of one run, is a
disk artifact that today's design deliberately avoids for run-secrets — env-only was the
choice precisely so a sealed value never lands on a filesystem. It also means the tofu run
gains a secret-file-staging step it does not have now (§1.2c), i.e. new plumbing on the
tofu driver, not just the ansible one.

| | C-env (Phase 2) | C-file (Phase 3, as shipped) |
|---|---|---|
| Secret on disk | never | 0600 under `/dev/shm` (RAM), one run |
| New code | capture hook seals N fields | capture hook + tofu-side file staging + `KUBECONFIG` export |
| Operator ergonomics | must map fields | paste a kubeconfig, get a path |
| Exposure surface | unchanged (env-only) | new: a RAM-only file holding a bearer |

**Decision:** the owner shipped **C-env first** (Phase 2 — no new exposure, unblocked the
persona) and then approved **C-file** (Phase 3) as the follow-on, because field
decomposition is too sharp an edge for the copy-paste `KUBECONFIG` case and for
exec-auth tools that read a file. C-file's exposure is bounded to a `0600` file under
`/dev/shm`, torn down (unlinked) once the subprocess that read it exits, with a startup
sweep removing any orphan left by a broken cleanup path — so the "secret on disk" row is
*RAM only, one run, never durable disk*.

---

## 3. Phased implementation plan (smallest first, each shippable)

Ordered so every phase is independently useful and independently reversible. Each names
the files/verbs it touches and whether it reuses item A/D machinery or is new.

**Phase 0 — the ansible-first chain, no capture (smallest).**
A `bootstrap` verb that composes `[ansible-play, tofu-apply]` on the existing Sequencer,
with no secret capture: the bootstrap play runs, then a plain apply. This alone exposes
the ordering the Sequencer already supports and covers the case where the tofu step needs
no bootstrap-produced credential (e.g. it reads its own `~/.kube/config` via a
pre-existing run-secret binding).
*Touches:* new `cmd/exocomp/bootstrap.go` (mirrors `handoff.go`).
*Reuses:* the Sequencer (`workflow.go`), `queue.Step`/`Spec`, `--wait`/`--source` wrap
(item A machinery), verbatim. *New:* one CLI file. Reversible: deleting the verb removes
the feature; nothing else changes.

**Phase 1 — the ansible-output capture → seal hook (the item-D mirror).**
`SecretKindAnsibleOutput`, an `AnsibleOutputSealer` mirroring `EnvelopeOutputSealer`, and
a post-play capture that reads a declared fact/fetched-file in-process and seals+binds it.
*Touches:* new `internal/executor/ansible_seal.go` (mirror of `tofu_seal.go`), a capture
reader in `internal/executor/ansible.go`, wiring in `cmd/exocomp/ansible.go`'s driver
build.
*Reuses:* the envelope seal/upsert, the run-secret binding model, the AAD shape, and the
redaction path (item D machinery). *New:* the capture surface (declared fact/fetched-file
reader) — there is no ansible equivalent today. Reversible: a nil sealer restores
drop-on-capture, exactly as the tofu path degrades with a nil `OutputSealer`
(`tofu_output.go:163-167`).

**Phase 2 — pre-bind the capture into the bootstrap chain.**
`bootstrap run --capture` publishes the `ab:<workflow>` binding up front and sets it as the
tofu step's `RunSecretBinding`, closing the loop end-to-end under **Option C-env**.
*Touches:* `cmd/exocomp/bootstrap.go` (bind + step wiring).
*Reuses:* the run-secret env resolution (`resolveRunSecrets`, ansible.go:65) and the
deterministic-name convention from the handoff (`handoff.go:184-187`). *New:* the
`ab:<workflow>` naming convention. Reversible: without `--capture` the verb behaves as
Phase 0.

**Phase 3 — `KUBECONFIG` file staging (LANDED, C-file chosen).**
`bootstrap run --capture-file NAME=FILE` delivers a captured value to the apply as a file
path instead of an env value: the daemon writes the sealed bytes to a `0600` file under
`/dev/shm` (`exocomp-secret-<random>`) and exports `NAME=<that path>`. The value never
reaches an argv, event, or the ledger; the file is unlinked once the subprocess that read
it exits, and a startup sweep removes any orphan a broken cleanup path left behind.
*Touches:* `internal/executor/runsecret_file.go` (the `StageRunSecretFiles` stager,
`StagedRunSecretFile.Close`, and `SweepOrphanRunSecretFiles`), `TofuOptions.RunSecretFiles`
+ its wiring in `tofu.go` and in `sandboxedTofu.Run` (`sandbox_drivers.go`, which grants the
staged path via `SandboxProfile.ReadFiles` so the sandboxed tier can read it too),
`AnsibleCapture.AsFile` and the `--capture-file` flag/composer wiring in
`cmd/exocomp/bootstrap.go`, the daemon read in `cmd/exocomp/serve.go` (which also runs the
startup sweep), and the doctor report in `cmd/exocomp/sandbox.go`.
*New:* filesystem staging of a sealed value — the one new exposure surface, bounded by the
hard requirement below. **The staged file never touches durable disk:** `/dev/shm` is
RAM-backed unconditionally, so there is nothing to assert or fail closed on — this is
strictly stronger than the original `noswap` `Tmpfs=` quadlet mount design, which does not
start at all under rootless podman (`the 'noswap' option is only allowed with rootful tmpfs
mounts`) and so never actually protected a rootless deployment. The bytes are additionally
mlocked best-effort (no rootless swap-avoidance option exists for tmpfs itself, so mlock is
the only lever left; a host that forbids it still stages, unlocked, with the residual
reported by `exocomp doctor`). Env-decomposition (Phase 2) is unaffected and needs no
`/dev/shm`.

*2026-08-24 (later): `/dev/shm` replaces the tmpfs run-dir mount.* The original Phase 3
design backed the per-job run directory itself with a `noswap` `Tmpfs=` quadlet mount and
asserted `statfs f_type == TMPFS_MAGIC` on it before staging, refusing otherwise. That mount
turned out to be unusable in the one deployment shape this project ships (rootless podman,
`UserNS=keep-id`): podman refuses `noswap` on a rootless tmpfs mount outright, so the
quadlet failed to start the container at all. `/dev/shm` needs no mount, no quadlet entry,
and no capability, is present unconditionally in every rootless container, and — unlike a
memfd-backed alternative that was spiked and rejected first (a memfd has no path a
sandboxed child's `--read-file` grant can name, because real `nono` closes inherited fds
before exec) — a `/dev/shm` file has a real path the sandboxed tier can grant. The per-job
run directory is ordinary storage on the data volume again; only the staged secret material
moved to RAM.

**Phase 4 (later) — generalize to `exocomp workflow` (Option B2)** if a third ordering
appears. Pure composition over the same Sequencer; deferred until two concrete orderings
justify the generic surface.

Item-A/D reuse vs new, at a glance: Phases 0 and 2 are almost entirely composition of
existing machinery; Phase 1 reuses the seal/bind/redact plane but adds the one genuinely
new primitive (an ansible capture surface); Phase 3 adds the only new exposure surface and
is decision-gated.

---

## 4. Security notes

- **Values-free everywhere.** The ansible sealer MUST NOT log, event, or otherwise
  surface the kubeconfig, exactly as the `OutputSealer` contract requires of sensitive
  tofu outputs (`tofu_output.go:159-174`) and as the capture summary carries names and
  counts but never values (`InventoryCapture`, `tofu_output.go:44-56`). The captured bytes
  are read in-process and never routed through the streaming event sink.
- **Reuse the redaction path.** The ansible run already builds a `secrets.NewRedactor`
  over the run's sealed material and vault-derived bytes (`cmd/exocomp/ansible.go:104-108`);
  a captured/bound value joins that same material set so any incidental echo in a later
  step's stream is scrubbed by the existing redactor, not a new one.
- **AAD unchanged.** The sealed row's AAD stays `name|kind` (`tofu_seal.go:83`); the only
  change is a new `kind` constant (`ansible-output`), so a ciphertext still fails to open
  against a moved name or a substituted kind. No new AAD construction, no envelope change.
- **Default-deny.** The capture seals only operator-**declared** targets; an undeclared
  sensitive fact is never captured. A missing sealer drops the capture rather than leaking
  it (mirror of `tofu_output.go:163-167`). Under C-env no secret touches a filesystem;
  under C-file the staged file is 0600 under `/dev/shm` — RAM, unconditionally, no mount
  or assertion required — and unlinked once the subprocess that read it exits, so the one
  new exposure the §2.3 decision is about is bounded to RAM for the lifetime of one run.
