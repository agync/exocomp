# Runner definitions (image build deferred; caps wired)

`exocomp runner` registers execution-environment **recipes** in the database. Registering
one stores a definition; it does not build an image or call podman. Every runner verb
repeats this plainly: **"registered for the deferred container tier; not yet executed."**
The shape follows [design/byo-runner.md](../design/byo-runner.md) §2, which also argues the
image-build deferral itself is correct, not just unfinished.

A runner is two independent things a definition carries: the (still deferred) image recipe,
and the (now wired) resource caps. `--memory-max`/`--cpu-quota` are enforced the moment a
job names the runner with `--runner <name>` — see
[Resource caps enforcement](#resource-caps-enforcement-opt-in-cgroup-v2) below — with no
image ever built. The rest of a runner's fields (`--base`, `--bootstrap-file`,
`--containerfile`, `--packages`, `--ansible-core`) stay inert until the container tier ships.

## Why register a runner before the build tier exists

The registry is the two-tier model dependencies already use: an operator can name a runner
once (`runner add`), and `runner import` loads GitOps-style `runner.yaml` specs from a
directory into the same database — all before there is a build tier to consume the image
half of a definition. A definition holds no secret material, so registering one is safe
with no gate: the queue and the approval gate only come into play once a job actually names
the runner with `--runner`, at which point its caps (not its image recipe) take effect.

## Registering a runner

```
exocomp runner add fedora-ee --base registry.fedoraproject.org/fedora-minimal:44 \
  --packages sshpass,rsync
```

Two mutually exclusive build models feed the **same** content hash, so switching between
them never changes a runner's identity:

- **Model A (`--mode cached`, default)** — `--base` plus `--bootstrap-file` (a script run as
  one layer) and/or `--packages` (sorted, deduplicated).
- **Model B (`--mode onthefly`)** — `--base` plus `--containerfile <path>`. Naming a
  Containerfile selects `onthefly` by default unless `--mode` is passed explicitly.

Other flags: `--ansible-core <version>` (a per-project ansible-core pin), `--memory-max` /
`--cpu-quota` (the resource ceilings a run asks for, per design §6, in systemd syntax — `2G`,
`200%`).

## Resource caps enforcement (opt-in, cgroup v2)

`--memory-max` / `--cpu-quota` are enforced by a per-job cgroup v2 subtree the daemon writes
directly (`memory.max`, `memory.swap.max=0`, `cpu.max`), **not** by systemd-run — the runtime
image ships no systemd. A job over its memory ceiling is OOM-killed (exit 137); a job over its
CPU ceiling is throttled. `memory.swap.max` is pinned to 0 so an over-limit job is killed
rather than silently swapped.

A job asks for a runner's caps with `--runner <name>` on the job-creation command:

```
exocomp playbook run site.yml --runner fedora-ee
exocomp workspace plan prod --runner fedora-ee
exocomp workspace apply prod --runner fedora-ee
exocomp run-cli kubectl --runner fedora-ee -- get nodes
exocomp command --host web --runner fedora-ee -- systemctl status nginx
exocomp ansible --host web -m ping --runner fedora-ee
```

Only the caps are read from the named runner — no image is built or referenced. `--runner`
is a **local** flag: a `--remote` plan or apply does not carry it yet (todo, see
[design/byo-runner.md](../design/byo-runner.md) §9 Phase 1).

Enforcement is **opt-in and fail-closed on two independent axes**, and a job that names no
runner is completely unaffected — this never tightens the default, capless path:

- **No delegated cgroup subtree.** The default rootless, `UserNS=keep-id` container gets no
  writable cgroup subtree, so caps are unavailable and a job that requests them is
  **refused, never run uncapped**. To enable, switch the quadlet to the owner-approved
  root-in-container model documented in `packaging/quadlet/exocomp.container`
  (`PodmanArgs=--systemd=always`, drop `UserNS=keep-id`).
- **The sandboxed tier itself is not active.** Caps only ever apply inside the
  `local-sandboxed` tier (the cgroup placement happens where the sandboxed drivers run); a
  job with caps that would otherwise fall back to the plain `local` subprocess boundary is
  refused for the same reason — an uncapped fallback would defeat the ceiling silently.
- **The named runner does not exist, or its stored `memory_max`/`cpu_quota` no longer
  parse.** Refused at dispatch, before the job runs, rather than silently treated as no
  caps at all.

The cgroup subtree itself is prepared **lazily**, the first time any job actually names a
runner with caps set — a fleet that never uses `--runner` never has its own cgroup layout
touched. Once prepared (or once found unavailable), the daemon reuses the same controller
(or the same refusal) for every later capped job in that process's lifetime.

`exocomp doctor` reports the state under **per-job caps**: "available" when a capped runner
would be enforced, or "unavailable — … a capped runner would be REFUSED (fail-closed)"
otherwise. Check it before enqueuing a capped runner. The daemon's own log also carries one
line the first time a capped job is dispatched, successful or not — the same signal, not
only on request.

`runner list` / `runner show <name>` / `runner rm <name>` round out the registry; `show`
prints the bootstrap script body on the text path (`--output json` never carries it — the
JSON projection stays body-free, since an arbitrarily long script would blow the envelope's
low-token contract for every listing; the response carries only its SHA-256 and line count).

## Importing specs from a directory

```
exocomp runner import ./runners
```

Reads every `*.yaml`/`*.yml` in the directory as a `runner.yaml` spec (with or without the
`runner:` wrapper design §2 shows), creating or updating the runner of that name. A
malformed or invalid spec fails the import at that file, before any later file in the
directory is applied. A `build.no_cache` field in an imported spec is accepted but has no
effect yet, and the import says so.

## Naming

A runner name is 1–64 characters, must start with a lowercase letter or digit, and holds
only `[a-z0-9._-]` — the shape an eventual image tag would need, enforced at registration
rather than left to fail later at a build that does not exist yet.

## Where a definition lives, and why

Definitions live in `internal/store` — the same database as sources, inventories,
schedules, and plan-policies — so a runner is backed up and audited with the data volume,
not held in a config tree the volume does not carry. `runner import` reads from a directory
outside the data dir but writes into it; nothing under `/etc` or `~/.config` holds runner
state (`~/.config/exocomp` is reserved for the remote CLI's own client-side host pins, the
opposite direction: a workstation reaching a daemon, not the daemon's own registry).

Related: [design/byo-runner.md](../design/byo-runner.md) for the full build-mode design,
the tooling decision (shell podman, never embed the Go bindings), and the phased plan for
if and when the build tier is actually built.
