# Design — Bring-your-own container runtime runner

## Status

The `container` tier stays deferred. The default and only shipped isolation is nono/Landlock
(`enforcing`); this document sets out why that is the right default and specifies the
exact shape a bring-your-own-runner (BYO-runner) would take if a concrete need justifies
building it.

One slice of the tier's value ships without the tier: per-job memory/CPU caps, enforced by
cgroup v2 direct writes with an opt-in root-in-container privilege model (§6). The rest of the
tier — BYO images, the podman socket, the build/cache machinery — remains deferred.

---

## 1. Vision and honest status

The vision people reach for when they say "BYO container runner" is: *let an operator hand
exocomp a bootstrap script (or a Containerfile) and have jobs run inside an image built from
it, with hard resource caps.* That is the `container` tier from
[execution.md](../architecture/execution.md).

**The central finding: exocomp has already made this architectural decision, and made it by
rejecting the podman-socket path for v1.** This is not a gap waiting to be filled — it is a
deliberate deferral with a shipped alternative. Verified against the docs and the tree:

- **The `container` tier is *designed but deferred*** (execution.md:34). It is not in the
  code: `internal/executor/sandbox.go:21-32` defines only `TierDisabled`, `TierEnforcing`,
  and `TierOpportunistic`. There is no `container` Tier constant. "Designed, deferred" is literally
  true.
- **[ADR 0005](../adr/0005-job-sandboxing-nono.md) adopted nono/Landlock precisely to
  capture most of the container tier's prize without the socket.** Its Context (0005:16-21)
  names the container tier's two costs as the disqualifier: it "costs `exocompd` a mounted
  podman socket (effectively host-user power: the sensitive capability moves rather than
  disappears)" and "puts registry-pulled execution-environment images on the restore path."
  Its Consequences (0005:44-47): "Most of the `container` driver's privilege-separation prize
  is captured … with **no podman socket handed to `exocompd` and no registry on the DR
  path**."
- **The invariants make the socket hostile.** AGENTS.md invariant 1 ("one operator, one
  container, no external database — ever"), invariant 6 ("nothing here runs sudo"), invariant
  7 ("disaster recovery never depends on a registry, a vendor, or this project's own code").
  Podman-in-podman and sidecar topologies both break invariant 1; a registry pull on restore
  breaks invariant 7.
- **uv-venv already delivers per-project dependency/tool versioning.**
  [ADR 0011](../adr/0011-project-dependency-resolution.md) +
  `internal/deps/venv.go`: each source resolves its declared deps into a hash-gated,
  offline-restorable venv at `cache/python/<source>/<manifest-hash>`, carrying a
  project-pinned `ansible-core`. This is the first half of what execution.md:133 attributes
  to per-project EE images — delivered without any OCI machinery.

**What already exists** (ship state):

| Capability | Mechanism | Where |
|---|---|---|
| Per-job filesystem isolation | nono/Landlock default-deny, kernel-enforced | ADR 0005, sandbox.go |
| DB/key/socket denial from jobs | default-deny + name-check guard + doctor canary | sandbox.go:72-80, execution.md:98-101 |
| Per-project dep/tool versioning | hash-gated uv-venv per source | ADR 0011, venv.go |
| Offline/DR-safe execution | Landlock is local; venvs hardlink-seeded, `--offline` | ADR 0005:47, venv.go:55-56 |
| Per-job network scoping (by port) | Landlock ABI v4 network rules | execution.md:85-89 |

**What BYO-runner would ADD** — and *only* these, because everything else is already covered:

1. **BYO execution-environment images** — an operator supplies a bootstrap script or
   Containerfile so jobs run with arbitrary baked-in **system packages** (the one thing
   ADR 0011:86 explicitly leaves out: "bring-your-own-runner is the post-v1 seam for
   arbitrary baked-in system deps") and independent ansible/collection pins as a full image
   rather than a venv.
2. **Hard per-job resource caps** — memory/CPU ceilings the kernel enforces (execution.md:147
   names this as the container tier's other remaining justification). This half needs no
   image and no socket: it ships as cgroup v2 direct writes (§6).

Net: with `enforcing` shipped and uv-venv delivering per-project deps, the container
tier's justification has **narrowed to BYO system-package images + hard caps** (execution.md:146-147).
That narrow remainder is what this document scopes.

---

## 2. Two runner models — one spec, two build modes

An operator wants one of two things. Both are expressible with a single runner spec and a
single hash-gate; they differ only in *when* the image is built.

### Model A — cached runner (bootstrap → build once, reuse)

Operator supplies a **base image + a bootstrap script / package list**. exocomp builds a
runner image **once**, tags it by a content hash, and reuses it across every run until the
inputs change. Optional scheduled rebuilds refresh the base digest.

- Best for the common homelab case: a stable runner used by many runs.
- Maps onto execution.md:129-131: **ansible via ansible-runner's native podman EE isolation
  (the AWX Execution-Environment pattern)** — ansible-runner *already shells podman* to run a
  play inside an EE image, so the cached runner image *is* the EE; exocomp builds it and hands
  its tag to ansible-runner. For tofu, `podman run <runner-image> tofu …`.
- Cache-hit is offline: `podman run --pull=never`. Only a cache *miss* triggers an
  (online) build.

### Model B — on-the-fly Containerfile (build every run, layer cache)

Operator supplies (or exocomp generates) a **Containerfile**; exocomp builds it **every run**
with `podman build --layers` so an unchanged Containerfile is a near-free layer-cache hit.

- Best when the recipe changes often (active development of the runner itself).
- Uses buildah/podman layer cache + BuildKit-style cache mounts (§7).

### One spec expresses both

```yaml
# .exocomp/runner.yaml  (repo-side default; per-source, like the dep manifests)
runner:
  mode: cached            # cached (default) | onthefly
  base: registry.fedoraproject.org/fedora-minimal:44   # resolved to @sha256 at build
  bootstrap: |            # Model A: script run as one RUN layer
    microdnf install -y sshpass rsync
  # OR, Model B:
  containerfile: .exocomp/Containerfile   # path; mutually exclusive with bootstrap
  packages: [sshpass, rsync]              # sorted, hashed; convenience over bootstrap
  ansible_core: "2.18.1"                  # optional per-project pin (else image default)
  build:
    no_cache: false       # CACHEBUST-style force-clean for debugging
  caps:                   # §6 — enforced via systemd scope, not the socket
    memory_max: 2G
    cpu_quota: "200%"
```

Both modes feed the **same `runner-hash`** (§7), so switching `mode` never forces a rebuild.
`bootstrap`/`packages` is Model A; `containerfile` is Model B; the presence of `containerfile`
selects `onthefly` semantics by default.

### Registration — server-level vs repo-side, DB-native

A runner may be **server-level** (registered once, shared by many sources) or **repo-side**
(the per-source `runner.yaml` above). The native home for a registered runner is the
**database** — the same place sources, inventories, schedules, and plan-policies already live
(`internal/store`), so it is backed up + audited with the data volume and reachable by the
reactive/audit machinery. This is a two-tier model mirroring how dependencies already work
(operator-registered vs repo-declared).

- **Registered / global (DB)** — `exocomp runner add <name> --base … --bootstrap …` (also
  `runner list|show|rm`). Named, operator-managed, shared across sources. A source references
  it by name. Best for a handful of standard homelab runners.
- **Repo-side (`.exocomp/runner.yaml`)** — per-source, self-contained, GitOps; travels with
  the code.

A source picks one; precedence: `--runner <name>` flag → the source's `.exocomp` declaration →
a workspace/global default runner.

On location: **`/etc/exocomp/runners` — no.** `/etc` implies a system-level, multi-user,
root-managed daemon; exocomp is single-operator, rootless, no-sudo, one-container (invariants
1/6), and has no `/etc` config model. **`~/.config/exocomp/runners/` — off-pattern:**
exocomp's state lives in the backed-up data dir (`XDG_DATA_HOME`), not `~/.config` — which is
used only for the remote CLI's *client-side* host pins/tokens (`hosts.toml`), the opposite
direction (a workstation reaching a daemon, not the daemon's own registry). For file-based /
declarative registration, prefer `exocomp runner import <dir>` loading YAML specs from a dir
**under the data dir** into the DB — GitOps ergonomics without splitting state across
`~/.config`. The runner **definition** (recipe) is backed up; the built **image** stays in the
rebuildable cache (§7), reconstructable from the definition + `runner-lock.json`.

---

## 3. Tooling — podman-first, shell the CLI

**Decision: shell `podman` / `buildah bud` / `podman build --layers` / `podman pull`. Do
NOT embed the buildah Go library, the containers/storage or containers/image libraries, or
the podman Go bindings. Do NOT use the docker/docker Go SDK.**

go.mod carries no docker / podman / buildah / containers/* / opencontainers / crun /
containerd dependency. Shelling the CLI keeps it that way.

Rationale:

1. **Invariant 3 makes correctness exit-code-decided.** The executor's `Spec`/`Driver` model
   already runs ansible and tofu as shelled subprocesses judged by exit code. A shelled
   `podman` fits the *exact same pattern* with no new heavyweight dependency.
2. **buildah/containers-storage as Go libraries are cgo-heavy** and pull a large transitive
   tree. Embedding them jeopardizes the `CGO_ENABLED=0` static-binary build
   (`packaging/Containerfile` build stage) and the ~500 MB working-image ceiling
   ([ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md):58) — for a tier that is deferred and
   may never ship.
3. **ansible-runner already shells podman for EE isolation** (execution.md:129-131), so
   exocompd's *own* podman surface is thin: build/pull a runner image, `podman run` a tofu
   job. Thin surface → shell it.

**Why the docker SDK does not work against podman.** Podman implements the **Docker REST
API** (e.g. `/v1.41/build`), not Docker BuildKit's proprietary gRPC protocol. The
`github.com/docker/docker` client's modern build path targets BuildKit, so builds fail or
fall back unpredictably against `podman.sock` — the same class of failure as compose
`dockerfile_inline` against podman.sock. Rejecting the docker SDK is a correctness
constraint, not a style preference.

Reserve the podman Go bindings (`pkg/bindings` over the API socket) **only** if a later need
for streaming/events arises that the CLI cannot express cleanly. Do not adopt them
speculatively.

---

## 4. The nesting decision — host-user rootless socket mount (option a), gated

**Decision: mount the *host user's* rootless podman socket
(`unix:///run/user/1000/podman/podman.sock`) into exocomp's container (option a), narrowly
scoped with SELinux `label=disable` on that mount only, gated so DR-path runbooks never touch
it. NOT podman-in-podman (b). NOT a sidecar (c).**

How exocompd drives podman to build+run runner containers, per topology:

- **(a) host-socket mount [chosen].** exocompd's one container talks to the host user's
  already-running rootless podman via the mounted socket. The EE build/run happens on the
  host's podman, where it belongs. Keeps **one** exocomp container (invariant 1). This is the
  topology execution.md:138 and platform-contract.md:11 already name as the container driver's
  mechanism, and the one ADR 0005 priced.
- **(b) podman-in-podman [rejected].** Nests userns/subuid-subgid mappings, is fragile and
  slow, and on enforcing SELinux the nested MCS labeling is a known pain surface. Adds no
  benefit over (a) and multiplies the failure modes.
- **(c) sidecar podman [rejected].** A second long-lived container **breaks invariant 1's
  one-container commitment** and reintroduces daemon/privilege surface **without removing** the
  socket-power problem — you still hand something the socket.

**Trade-off, stated honestly.** Option (a) is genuinely host-user-equivalent power inside
exocompd's container — ADR 0005's exact objection (0005:18, execution.md:138: "the sensitive
capability moves rather than disappears"). Any code path reaching the mounted socket can run
arbitrary host-user containers. Therefore:

- The socket mount is **opt-in with the container tier**, off by default.
- It is kept **off the DR path** and **away from job subprocesses** (nono denies jobs socket
  access anyway — sandbox.go grants no socket; execution.md:107-109 confirms no podman socket
  is mounted today).
- Enabling it **materially lowers exocomp's own threat tier**, and should be treated and
  documented as such. `exocomp doctor` asserts the socket is reachable *only* from the
  container tier's build/run path and never from a job profile.

This decision is *coupled* to the sandbox's only guarantee against local sockets: Landlock
cannot refuse `connect(2)` on a pathname `AF_UNIX` socket (execution.md:103-112, ADR 0005:58-65),
so the sandbox's socket denial holds **by placement** — exocomp mounts no podman socket today.
Turning on the socket mount directly attacks that placement mitigation. The two decisions must
be made together, not independently.

---

## 5. SELinux / AppArmor / rootless on enforcing Fedora

The target is Fedora/RHEL **enforcing**; no example may depend on permissive mode
(platform-contract.md:21-22). The anchors are platform-contract.md:33-36 and the
volume-relabel prior art.

- **`label=disable` on the socket mount ONLY.** The deferred container driver's podman-socket
  mount needs `--security-opt label=disable` (or Quadlet `SecurityLabelDisable`) on *that
  mount* (platform-contract.md:35-36). **Do NOT blanket-disable labels on the whole
  container** — that would remove container-selinux's socket confinement, which
  platform-contract.md:33-34 and ADR 0005:55 rely on as a *second, independent* enforcement of
  "jobs reach no local sockets." Getting this wrong yields either broken jobs (forgot it →
  AVC denials that look like generic podman failures) or weakened isolation (over-applied).
- **`:z` / `:Z` on bind mounts.** Per-job workspace/data bind mounts need `:Z` (private
  relabel) or `:z` (shared) on enforcing Fedora (platform-contract.md:25).
  **Caveat:** `:Z`/`:z` apply `container_file_t` **only at container start**, not to files
  added later, and **`:Z` fails on NFS/CIFS** — "the NAS leg is the sharp edge"
  (platform-contract.md:27-28). This constrains where
  the image/build store lives (§7).
- **Rootless userns / subuid-subgid.** Rootless podman requires `loginctl enable-linger` and
  configured `/etc/subuid` + `/etc/subgid` ranges for the operator.
  For host-volume sharing, use `--userns=keep-id:uid=1000,gid=1000` (keep-id preferred
  over entrypoint chown scripts) so runner-produced files map back to the operator's uid
  without ACL surgery. `exocomp doctor` verifies subuid/subgid ranges and linger *before* the
  first job, mirroring how sandbox.go `Probe`/`RunCanary` assert rather than assume.
- **Run-secrets out of argv and correctly labeled.** The `disabled` tier already
  writes secret material (vault password, backend bearer) into a **per-job 0700 private
  directory**, never argv/env (execution.md:46-49, 158-160). BYO-runner must preserve this:
  resolved secrets go to a private dir bind-mounted into the runner container `:Z`, passed via
  `--secret` or a file path — **never** on the `podman run` argv (invariant 4; the process
  table is world-readable). ansible-runner's `env/` + `--secret` plumbing is the existing seam.
- **Hardening the runner container.** `--read-only` rootfs with a tmpfs for
  scratch; `--security-opt no-new-privileges`; keep the default seccomp profile (do not
  `--privileged`, do not `--security-opt seccomp=unconfined`); `--cap-drop=ALL` and add back
  only what a play needs (usually nothing); `--network` scoped to match the Landlock
  per-kind port story (SSH for ansible, HTTPS for tofu).

---

## 6. Resource caps without the socket — cgroup v2 direct writes

The one thing nono + uv-venv does not provide is **hard per-job memory/CPU caps**
(execution.md:147). It needs no podman socket, and it ships today.

The naive approach — `systemd-run --user --scope` under the rootless user slice — does not
apply inside exocompd's own container: the runtime image
(`registry.fedoraproject.org/fedora-minimal:44`, `packaging/Containerfile`) carries no
`systemd` package, so there is no `systemd-run` binary and no `systemd --user` manager, and
`packaging/quadlet/exocomp.container` starts no PID 1 to hand a scope to. Caps are therefore
enforced by the daemon writing cgroup v2 files directly (`internal/executor/cgroup.go`).

Mechanism, per opted-in job:

1. **Once at startup** the daemon prepares a delegated subtree. cgroup v2's *no-internal-
   process* rule forbids a cgroup from both holding processes and distributing controllers,
   so the daemon **vacates** its own base cgroup — moves every pid in `cgroup.procs` into a
   child `exocomp.svc` leaf — and only then writes `+memory +cpu` into the base's
   `cgroup.subtree_control` (skipping the vacate yields `EBUSY` on that write). An
   `exocomp.jobs` parent is created and made a distributor too.
2. **Per job** it `mkdir`s a child under `exocomp.jobs`, writes `memory.max`,
   `memory.swap.max=0` (**required** — without it a job over `memory.max` swaps instead of
   being OOM-killed, so the ceiling throttles rather than enforces), and `cpu.max`, then
   places the job process into it at fork via clone3 `CLONE_INTO_CGROUP` (Go's
   `SysProcAttr.UseCgroupFD`), so the ceiling is in force from the process's first
   instruction. Teardown `cgroup.kill`s the subtree and `rmdir`s it with the job.
3. **Fail-closed.** Caps requested with no delegated, writable subtree are refused, never run
   uncapped — the same "never silently" posture ADR 0005 holds for the sandbox tier.
   `exocomp doctor` reports availability under "per-job caps".

### A job selects caps by naming a runner

A job names a runner purely for its caps via `--runner <name>` (there is no job→runner
*image* linkage — the container tier stays deferred, §1). `store.JobArgs`/`queue.Spec` carry
`RunnerName` as a JSON field on the existing `args_json` column (no migration). `--runner` is
accepted on `playbook run`, `run` (the hooked workflow), `workspace plan`/`apply`, `run-cli`,
`command`, and `ansible`; `--remote` carries it for the two verbs with a remote form
(`workspace plan`/`apply`, `playbook run`), so a remote-submitted job takes the same
dispatch-time path as a local one.

At dispatch — the daemon's per-kind drivers and every no-daemon inline path alike —
`resolveRunnerCaps` looks the named runner up and runs its `memory_max`/`cpu_quota` through
`ParseCaps`, refusing the job if the name or the caps do not resolve. `withCaps` then sets
`SandboxOptions.Caps` and prepares the `*executor.CgroupController` lazily (via `sync.Once`)
— only the first time a job actually asks for caps, so a fleet that never uses `--runner`
never has the daemon's own cgroup rearranged. Two fail-closed axes guard enforcement:
`applyJobCaps` refuses when the delegated subtree is not writable, and
`requireSandboxedForCaps` refuses when the effective tier is not `enforcing` — a plain
subprocess never reads `SandboxOptions.Caps`, so a capped job that fell back to it would
otherwise run uncapped. `parseMemorySize` guards against integer overflow on a large digit
string that would otherwise wrap to a wrong tiny cap.

### Enforcement and privilege model

Enforcement is verified on real podman (5.8.4 host, 5.8.1 lima), not assumed: a 200 MB
allocation under a 50 MB cap is OOM-killed (`memory.events oom_kill 1`, SIGKILL → 137); a CPU
workload under a 10%-of-one-core `cpu.max` throttles (`cpu.stat nr_throttled 33`).

The privilege model is opt-in **root-in-container**: `PodmanArgs=--systemd=always` mounts
`/sys/fs/cgroup` read-write and runs PID 1 as uid 0 inside the rootless user namespace (no
host root). The rootless-preserving alternative, keep-id delegated non-root, does not work on
podman 5.8.x — a `--userns=keep-id` non-root process is denied writes to `/sys/fs/cgroup`
even with `--systemd=always`, and making it work needs explicit cgroup ownership delegation
podman does not grant by default. The default deployment stays rootless + keep-id and
**capless**; caps are opt-in.

---

## 7. Cache and lifecycle — port the uv-venv model to OCI images

The BYO-runner cache should be a **near-mechanical port** of the ADR 0011 uv-venv cache. That
cache is hash-gated on manifest content, keyed under the backed-up data volume, fail-closed,
and offline-restorable (`internal/deps/venv.go`). Reuse its exact shape.

### 7.1 Hash-gate and ready-marker

- **runner-hash** = `sha256("runner-v1" \x00 canonicalJSON{ base:"<registry>@sha256:…",
  bootstrap_sha256, packages:sorted([...]), ansible_core, python, … })[:32]`. This ports
  `hashManifest` verbatim (manifest.go:137-143: kind/version prefix + NUL + canonical bytes,
  sha256, first 32 hex). Bump `"runner-v1"` when the recipe format changes so old images never
  collide onto new semantics (same reason `kind` is mixed into the venv hash).
  **Fail-safe carries over**: over-resolving on an unrelated edit is safe; under-resolving on a
  real change is not.
- **Ready-marker = OCI label.** The venv's `.exocomp-venv-ready` marker file
  (venv.go:37-38, written only after a clean provision) maps to an OCI **label**
  `dev.exocomp.runner.hash=<runner-hash>` baked at build. Cache-hit test =
  `podman image exists exocomp-runner:<hash>` **AND** the inspected label equals the
  recomputed hash. A crashed build leaves a dangling image with no matching label → rebuilt,
  exactly the partial-venv removal semantics (venv.go:106-110, markerMatches venv.go:260-266).

### 7.2 Base pinned by digest

The `Containerfile` **pins base images by mutable TAG** today
(`fedora-minimal:44`, `golang:1.26`, `uv:0.9.7` — Containerfile:1,8,16,57,78) while pinning
subprocess *tools* by sha256 (`tool-digests.txt`) and freezing versions into `MANIFEST.json`.
**BYO-runner must not inherit this flaw.** A runner-hash computed over a *tag* is not
reproducible — the same tag resolves to different bytes over time, so an "unchanged" runner
silently drifts. **Resolve tag→digest at build, write `FROM …@sha256:…`, feed the DIGEST (not
the tag) into runner-hash, and record it in a `runner-lock.json`** beside `MANIFEST.json`
(base digest, bootstrap sha, package list, **resolved** package versions, toolchain pins).
`microdnf install <pkg>` without pinned versions drifts even at a fixed base digest, so the
lock must record resolved versions or "reproducible" is skin-deep.

### 7.3 Build-per-run layer cache (Model B)

- Always `podman build --layers` (buildah caches intermediate layers).
- BuildKit-style cache mounts: `RUN --mount=type=cache,id=uv,target=/root/.cache/uv` and
  `id=dnf,target=/var/cache/dnf` so a rebuild after a bootstrap edit reuses base layers and the
  uv/dnf download caches.
- **Stage order: base → dnf packages → uv-cache warm → bootstrap LAST** (most-edited layer
  last), mirroring the Containerfile's own `COPY go.mod` → `go mod download` → `COPY source`
  ordering (Containerfile:3-5) that keeps the download cached.
- `runner.build.no_cache` forces a clean build (CACHEBUST precedent).

### 7.4 Freshness — into the existing cron scheduler, no new subsystem

`internal/sched` fires recurring jobs into the **one** queue (invariant 5). Today's kinds are
shell/backup/tofuplan (`store/schedule.go:15`, `sched/driver.go:17-19`). Add a
**`ScheduleRunnerRefresh`** kind: re-resolve each declared base tag→digest and rebuild **iff**
the digest moved or the runner-hash changed — a fast no-op otherwise (the venv "stable hash is
a fast no-op" property). Default cadence **weekly**. Reuses `ValidateCron`/`fire`/
`RecordScheduleFire` (sched.go) with no new machinery, and a **concurrency key per runner-hash**
(as schedules already carry `ConcurrencyKey`, sched.go:190) so two builds of the same runner
never race.

**No CVE-feed rebuild trigger.** Invariant 7 forbids depending on an external vendor feed, and
it would reinject network into the steady state ADR 0011 removed. The **base-digest refresh IS
the CVE mechanism** — Fedora ships CVE fixes in the base, so pulling a moved digest is the patch
path. `exocomp doctor` surfaces the running runner's base digest + age for an operator to force
a rebuild (mirroring how doctor diffs the toolchain against `MANIFEST.json`, ADR 0006).

### 7.5 GC / prune — one job, both caches (closes an existing gap)

restic backups are bounded (`forget --prune`, backup/restic.go:135) and tofu state is bounded
(`PruneTFStates`, store/tofu.go:262-287), but **`cache/python` is unbounded today** — venvs
accumulate one dir per manifest hash per source forever, with no reaper. A runner image store
would compound this. Ship **one bounded-cache prune job covering BOTH**:

- `runner.cache.keep = 3` most-recent per source, **plus** keep-any-used-within = 30d.
- `runner.cache.max_gb = 10` ceiling; on overflow evict LRU, then `podman image prune -f` +
  `buildah prune` for dangling build cache.
- Apply the **identical keep policy to `cache/python`**, closing the gap.
- Run as a scheduled queue job on the restic-forget cadence.

### 7.6 Storage placement and DR

- **Image/build store on a LOCAL relabelable path** (`~/.local/share/containers` or a local
  data-volume subdir), **never the NAS leg** — `:Z` fails on NFS/CIFS (§5). A mislabeled or
  NFS-backed store yields intermittent EACCES/AVC that look like build flakes.
- **Do NOT back up the image store.** Back up **`runner-lock.json` only** — images are
  rebuildable-from-lock, so backing them up wastes the restic budget. (Contrast venvs, which
  *stay* backed up because they are not trivially rebuildable offline.)
- **Online-build, offline-run, off the DR path.** A runner-hash **hit** runs
  `podman run --pull=never` — no network (mirrors the venv `--offline` hit, venv.go:55-56).
  Only a **miss** triggers an online build (a build fundamentally needs the base + a package
  index). BYO-runner runbooks are **refused on the DR path** by the same doctor gate that pins
  DR to the image toolchain (execution.md:140-142, ADR 0011:79-80). A registry pull on restore
  breaks invariant 7 — enforce this gate, do not merely document it.

### 7.7 Build is operator-initiated, never a job side effect

Any build needs podman/buildah = the exact privileged surface ADR 0005 excluded. So the runner
**build** must be **operator-initiated, approval-gated, and doctor-visible** — framed like the
ADR 0006 escape hatch ("operator-initiated, at a shell, on purpose"), **never** triggered
implicitly by a sandboxed job (whose profile grants no socket, sandbox.go:72-80). The mitigation
is a *process* control, so it needs an explicit doctor assertion, not just a convention.

---

## 8. Recommendation

1. **Keep the status quo.** nono/Landlock (`enforcing`) stays the **default and only
   shipped** isolation. It is the correct podman-first-yet-no-podman-socket answer for a
   single-container, no-sudo, DR-offline system, and it keeps go.mod free of container
   dependencies. Do not build the container tier speculatively.
2. **Build BYO-runner only when a concrete need justifies it** — either (a) a real BYO
   execution-environment requirement (arbitrary baked-in **system packages** that uv-venv
   cannot provide, ADR 0011:86), or (b) **hard per-job resource caps**. If the need is caps
   only, **build §6 (systemd scopes) instead of the container tier** — it is far cheaper and
   does not lower the threat tier.
3. **When built, use exactly the shape above**: shell podman/buildah (§3), host-socket mount
   option (a) with `label=disable` on that mount (§4), the uv-venv-mirrored hash-gated cache
   with base-by-digest (§7), operator-initiated builds off the DR path (§7.7).

### Open questions

- **Is there a real need at all?** If no runbook needs system packages, and caps (§6) cover
  the resource case, the tier need not be built and §1's table stands.
- **How do EE images map to sources?** One runner per source (like venvs), or a shared runner
  pool? Affects the cache keyspace and prune policy.
- **How is the socket mount fenced in code** so no job profile can reach it and the DR gate is
  enforced, not documentary? (execution.md:142 states intent; the enforcement is unbuilt.)
- **Model A vs Model B default** — `cached` proposed.
- **`runner-lock.json` schema and location** relative to `MANIFEST.json`.

---

## 9. Phased implementation plan (dependency-ordered, one phase each)

Each phase is a self-contained unit of work. Phases 0–2 are worth doing **even if the
container tier is never built**, because they close real gaps in the shipped system. Phase 1
is done; the rest are a proposed order if the tier is built.

- **Phase 0 — Bound the existing caches (no container tooling).** Add the `cache/python` prune
  job (§7.5) as a scheduled queue job on the restic-forget cadence. Closes an unbounded-growth
  gap that exists **today**. Depends on nothing.
- **Phase 1 — per-job resource caps via cgroup v2 direct writes (done).** The daemon writes
  `memory.max`/`memory.swap.max=0`/`cpu.max` into a per-job child of its own delegated cgroup
  subtree and places the process with `CLONE_INTO_CGROUP` (`internal/executor/cgroup.go`).
  Opt-in and fail-closed; the privilege model is root-in-container
  (`PodmanArgs=--systemd=always`). A job selects caps by naming a runner (`--runner`). Full
  mechanism, wiring, and enforcement in §6.
- **Phase 2 — doctor prerequisites probe for the container tier.** Add probes (rootless podman
  present, user socket reachable, subuid/subgid configured, SELinux booleans/label posture)
  that **assert rather than assume**, mirroring sandbox.go `Probe`/`RunCanary`. Gates every
  later phase and surfaces "can this host even do BYO-runner?" before any code touches podman.
- **Phase 3 — runner-hash + runner-lock (pure Go, no podman calls yet).** Port `hashManifest`
  to `runner-hash`, define `runner.yaml` parsing and `runner-lock.json` emission, base
  tag→digest resolution recorded but not yet built against. Testable in isolation. Depends on
  P2 for the digest-resolution probe.
- **Phase 4 — Model A cached build via shelled podman/buildah + host-socket mount.** Wire the
  socket mount (option a, `label=disable`), `podman build` from bootstrap, OCI ready-label,
  cache-hit via `podman image exists` + label check, `podman run --pull=never` on hit. This is
  the first phase that lowers the threat tier — gate it opt-in, off the DR path. Depends on
  P2+P3.
- **Phase 5 — ansible-runner EE integration + tofu `podman run`.** Hand the cached runner image
  to ansible-runner as its EE (AWX pattern); `podman run <image> tofu …`. Secrets via private
  dir + `--secret` (§5), hardening flags (§5). Depends on P4.
- **Phase 6 — Model B on-the-fly build.** `podman build --layers` + cache mounts + stage
  ordering + `no_cache` knob. Depends on P4 (shares the build plumbing).
- **Phase 7 — freshness + GC for images.** `ScheduleRunnerRefresh` kind (weekly base-digest
  refresh, no CVE feed) + extend the Phase-0 prune job to cover the image store and build
  cache. Depends on P4 (needs images to refresh/prune) and reuses P0's prune job.
- **Phase 8 — DR-gate enforcement.** Make the doctor gate that refuses BYO-runner runbooks on
  the DR path **enforced, not documentary** (execution.md:142). Depends on P4/P5 existing to
  gate.
