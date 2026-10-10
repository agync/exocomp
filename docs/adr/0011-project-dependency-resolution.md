---
type: Decision
title: "ADR 0011 — Project dependency resolution: uv at runtime for declared manifests"
description: "Project dependencies resolve from root requirements.txt or pyproject.toml into hash-gated caches, with uv shipped at runtime."
status: stable
tags: [decision, dependencies, offline]
---

# ADR 0011 — Project dependency resolution: uv at runtime for declared manifests

Superseded by [0015](0015-per-source-collections.md) for the collections decision; the venv, tofu-provider, tofu-modules, and system-package decisions stand.

## Status

Accepted · 2026-08-22 — refines the boundary in [ADR 0006](0006-uv-toolchain-escape-hatch.md).

## Context

A real playbook rarely runs on ansible-core alone. Modules pull Python libraries the
controller must have — `proxmoxer` for Proxmox, `netaddr`/`dnspython` for networking,
`boto3` for AWS-shaped APIs, `jmespath` for `json_query`, provider SDKs for dynamic
inventories. Collections (galaxy) and tofu providers/modules are the other two dependency
planes. Two constraints pull against each other:

- **Deps must be MET** — a run cannot fail because `proxmoxer` is absent.
- **Deps must NOT be re-fetched every run** — pulling the world on each job is slow and
  reintroduces the network dependency the offline-cache work removes.

Operators declare Python dependencies the ordinary way: a **`requirements.txt`** or a
**`pyproject.toml` (PEP 621 `[project.dependencies]`, `[dependency-groups]`) at the project
root** — the files their editor, CI, and `uv` already understand. exocomp should honour
those, not demand a bespoke manifest.

This bumps a boundary in ADR 0006: *"the control plane never invokes uv … no on-demand
installs on the job path."* That rule exists to keep the ansible-core **toolchain switch**
an operator-initiated act with no registry on the DR path. It was not written to forbid
exocomp from resolving a project's *declared* dependencies — ADR 0006 itself (Consequences)
anticipates per-project venvs. This ADR draws the line precisely.

## Decision

**exocomp resolves a source's declared dependencies into hash-gated, offline-restorable
caches, and MAY invoke uv to do so — for project dependencies only, never for the core
toolchain.**

Manifest discovery (repo-side default; `.exocomp/` is the explicit override, per the
source model):

- **Python** — project-root `pyproject.toml` (PEP 621 deps / `[dependency-groups]`) or
  `requirements.txt`; `.exocomp/requirements.txt` overrides. Resolved by **uv** into a
  managed venv at `cache/python/<source>/<manifest-hash>` under the data-volume toolchain
  dir; the ansible driver points `ansible_python_interpreter` (and PATH) at it.

**Execution model — every ansible run executes inside a uv-managed venv scoped to its
source (project/workspace).** The run is never launched against the bare shared
interpreter; the driver always activates a uv venv keyed to the source + manifest hash.
That venv carries `ansible-core` (project-pinned; default = the image toolchain pin from
ADR 0006) + `ansible-runner` + the source's declared Python deps. Per-source isolation is
cheap because uv seeds each venv from its shared package cache by hardlink (no re-download,
offline once cached), and the manifest hash means an unchanged project reuses its venv as a
no-op. A source that declares NO extra deps resolves to the shared base toolchain venv (also
uv-managed — there is nothing project-specific to isolate); the moment it declares one, it
gets its own. This gives the homelab/hybrid case what it wants — a playbook can pin its own
ansible-core and libraries without disturbing another project — while the **DR-path runbooks
(backup, restore drill, host onboarding) still execute the image-pinned toolchain, never a
project venv** (ADR 0006: a fresh restore has the image before it has the volume).
- **Ansible collections** — `.exocomp/requirements.yml` (or a repo-root `requirements.yml`)
  vendored into `cache/collections` (already built; `ANSIBLE_COLLECTIONS_PATH`).
- **Tofu providers** — the existing plugin cache + `filesystem_mirror` (already built).
- **Tofu modules** — a persistent per-workspace module cache so registry/git modules are
  not re-fetched every `init`.

**Hash-gated:** a manifest's content hash decides whether to re-resolve. Unchanged manifest
→ reuse the cache → resolve offline. This is what makes "met, but never fresh every run"
true and keeps the network off the steady-state job path.

**uv ships in the distribution.** It is already in the runtime image (Containerfile runtime
stage + `MANIFEST.json` pin `uv==0.9.7`); this ADR makes that load-bearing, not incidental.
For a host (non-container) install, uv joins the pinned runtime tool set alongside
ansible-core/tofu/restic/age/sops, and **`exocomp doctor` probes it** (today it probes
ansible/tofu/restic/age/sops but not uv/git — a later pass adds them).

Boundary refinements to ADR 0006 (what stays true, what changes):

- **UNCHANGED — the toolchain switch stays operator-initiated.** exocomp never runs
  `uv tool install ansible-core==Y` on the job path; swapping ansible-core is still a human
  act at a shell (ADR 0006).
- **UNCHANGED — the DR path is pinned to the image.** Backup, restore-drill, and host-
  onboarding runbooks execute the baked-in image toolchain and image-pinned deps, never a
  volume-resolved project venv: a fresh restore has the image before it has the volume.
- **CHANGED — the control plane MAY invoke uv for project-dependency vendoring**, hash-gated
  and offline-preferring, as a run-lifecycle step. This is distinct from the forbidden
  on-demand toolchain install: it resolves a project's *declared* deps into a cache, not the
  interpreter/ansible-core itself, and it does not touch the network when the hash is stable.
- **System packages remain out of runtime scope** — the host provides them or the operator
  bakes a container layer; bring-your-own-runner is the post-v1 seam for arbitrary baked-in
  system deps.

## Consequences

- A vendoring step (`[pre-run] resolve deps if manifest hash changed`) becomes part of the
  run lifecycle, expressible as a step-ledger step; on a stable hash it is a fast no-op.
- Project venvs live in the data-volume toolchain dir → backed up, offline-restorable,
  drill-covered — the same guarantees ADR 0006 gives the core toolchain.
- uv is now a declared runtime dependency, not a build-only tool; doctor gains uv (and git)
  probes and the bill of materials lists uv as runtime.
- The `<manifest-hash>` venv path means switching a dependency set is a new cache entry, not
  a mutation — an old venv is reusable on rollback, matching the upgrade/rollback model.
- A malformed or unresolvable manifest fails the vendoring step loudly (fail-closed), so a
  run never proceeds against half-met dependencies.
- **`pyproject.toml` is parsed with a real TOML parser** (`github.com/BurntSushi/toml`, a
  zero-dependency, widely-used library), not a line scanner: the control plane collects the
  `[project].dependencies` array and every `[dependency-groups]` group array, and hands
  those specifiers to uv. A PEP 735 `include-group` entry is skipped (it names a group, not
  a package) and `[project.optional-dependencies]` extras are excluded (opt-in, not
  installed by default). A structurally broken `pyproject.toml` is an error, so it fails the
  vendoring step rather than silently resolving to fewer dependencies.
