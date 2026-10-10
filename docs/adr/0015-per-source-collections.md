---
type: Decision
title: "ADR 0015 — Per-source ansible collection isolation"
description: "Ansible collections go per-source in a content-hash-keyed cache resolved at claim time and garbage-collected with the source lifecycle."
status: stable
tags: [decision, ansible, offline]
---

# ADR 0015 — Per-source ansible collection isolation

## Status

Accepted · 2026-09-06 — supersedes [ADR 0011](0011-project-dependency-resolution.md)'s
collections decision; the venv decision in ADR 0011 stands.

## Context

The dependency model has three planes with three scopes:

| Plane | Cache | Scope | Versions per item | Lifecycle GC |
|---|---|---|---|---|
| Python deps (uv venvs, ADR 0011) | `cache/python/<source>/<hash>` | **per-source** | per-source, hash-gated, concurrent | `PruneVenvs` (keep-N + since) |
| Ansible collections | `cache/collections/` (flat) | **shared** | **one per collection, global** | none |
| Tofu providers | `cache/tofu/plugin-cache` (+ per-workspace modules) | shared providers / per-workspace modules | one per provider | none |

The venv plane is the model we already built and want everywhere: a per-source
namespace keyed by `<source>/<content-hash>`, ready-marker-gated (unchanged →
offline reuse), retention-pruned, and resolved at claim time in the daemon's
ansible driver (`cmd/exocomp/ansible.go`) so every face (CLI, web, API, MCP)
gets it uniformly.

Collections are the one non-isolated plane. A single flat dir is appended to
every run's `ANSIBLE_COLLECTIONS_PATH`; `ansible-galaxy` installs by name, so a
second version overwrites the first — two sources cannot pin different versions
of one collection. Vendoring is manual-only (`playbook vendor`, which requires
an explicit `--requirements` flag); nothing hooks source add/sync/run; nothing
is pruned. A requirements-file discoverer already exists
(`internal/deps/galaxy.go`: `.exocomp/requirements.yml` override else root
`requirements.yml`, content-hashed, sharing `FindProjectRoot` with the Python
manifest) but today has **no production caller** — `playbook vendor` never
invokes it.

The operator's requirement: **unify the isolation model** — make collections
behave the way venvs already do.

## Decision

Apply the venv model to collections: a **per-source, content-hash-keyed
collections dir**, resolved at claim time, with lifecycle GC. This supersedes
ADR 0011's shared `cache/collections` vendoring decision.

### Layout

```
cache/collections/<source>/<requirements-hash>/
    ansible_collections/<ns>/<name>/...
cache/collections/<source>/<requirements-hash>/.exocomp-galaxy-ready
```

`<source>` is `deps.SourceKey(manifest.Root)` — the same slug+hash namespace a
venv uses — so a source's venv and its collections live under one coherent key.
The shared `cache/collections` dir survives only as a **fallback** for
collections no source declares; per-source wins by path order. Once `playbook
vendor` becomes source-keyed, nothing writes the shared dir anymore — it is a
legacy fallback for upgrades and operator-placed collections, kept readable so
the offline guarantee never regresses for an existing deployment.

### Discovery

`.exocomp/requirements.yml` overrides a repo-root `requirements.yml`, matching
the Python manifest precedence (already implemented in `internal/deps/galaxy.go`;
this ADR adds the `Root`/namespace plumbing and the first production callers).

### Vendoring = resolve at claim time

Mirroring the venv resolver, the ansible driver resolves the source's
collections before the play runs: on a matching ready marker it is a no-op with
no Galaxy dial; on a new hash it runs `ansible-galaxy collection install -r
<req> -p cache/collections/<source>/<hash>` as a subprocess of the claimed job
(fail-closed — an unresolvable requirements file fails the run). The daemonless
inline drain path resolves the same way before enqueue, matching the venv's two
call sites. The explicit `playbook vendor` command stays as the manual
queued-job path, now source-keyed. This is the "vendoring step, part of the run
lifecycle" ADR 0011 anticipated and the same pattern `offline-caches.md` already
documents for the venv resolver: it is a subprocess of a queued job, not a new
execution path, and an inline dial is bounded by the playbook job's own timeout.

Under the sandbox tier the `ansible-galaxy` invocation runs wrapped in the same
resolver-sandbox profile uv uses (denied the data dir, keys, and database;
granted write on the per-source collections dir, read on the requirements and
tool tree, HTTPS egress) — `ansible-galaxy` and its galaxy-server interaction
are as untrusted as a manifest's build backend. Like uv's download cache, the
galaxy download/temp cache needs an explicit daemon-pre-created, granted write
path (Landlock grants attach only to existing paths). The cache lives at
`<data dir>/cache/galaxy`, a sibling of the collections cache root, and is
split so the resolver's `HOME` — `<data dir>/cache/galaxy/home` — is
**never granted**: nono keeps its own protected state root under `$HOME` and
refuses to launch when a grant would let the sandboxed process reach it, so a
grant covering the home dir (the earlier `cache/collections/.galaxy-cache`
layout) failed every sandboxed resolve. Only the directories galaxy actually
writes — `ANSIBLE_HOME` (`~/.ansible`, which carries `GALAXY_CACHE_DIR` and
`DEFAULT_LOCAL_TMP`) and `TMPDIR` — are granted, so a fresh provision under
the enforcing tier writes its download/temp tree exactly where the kernel
allows.

### Resolution

The driver appends the per-source collections dir **ahead of** the shared
fallback so a source's pin wins. The per-job sandbox read-grant must **split**
the `CollectionsPath` value on `os.PathListSeparator` and grant each component
— the current single-entry grant (`internal/executor/sandbox_drivers.go`) would
name one nonexistent joined path. This split restores grant *correctness*; it
does not give per-source read *isolation*: the base job grant
(`cmd/exocomp/sandbox.go`) already reads the `cache/collections` parent for
every sandboxed ansible job, so any job can still read any source's collections
dir. The base grant stays (it is what keeps the legacy fallback readable); the
per-run grant ensures the per-source path is readable at all.

### Lifecycle GC

- **Source delete** — after `DeleteSource`, remove that source's
  `cache/python/<source>` and `cache/collections/<source>` — but **only when no
  other registered source resolves to the same root** (a shared root means a
  shared `SourceKey`; purging would break a sibling).
- **Source edit (requirements hash changes)** — a new dir is provisioned; old
  hashes for that source are pruned with the same keep-N + since policy as
  `PruneVenvs`.
- The shared fallback dir is never purged by a single source's lifecycle.

### Scope exclusions

Galaxy **roles** are out of scope: they install through a different binary,
resolve through `roles_path`, have no cache today, and the ecosystem moves roles
into collections. A collection that *contains* roles is fully in scope. Roles get
their own ADR if a need appears. Keeping a **shared fallback** for undeclared
collections is part of the decision (see Layout).

### System packages

Unchanged from ADR 0011: out of runtime scope — the host or a container layer
provides them.

## Consequences

- Two sources can pin different versions of one collection; each resolves
  offline from its own hash-keyed dir.
- A source's collections travel with its venv under one `<source>` key — one
  GC story, one backup story.
- Galaxy is dialed only on a hash change (or an explicit vendor), preserving the
  offline-run guarantee (invariant 7).
- The per-run sandbox grant splits `CollectionsPath` and grants each component;
  the base job grant keeps `cache/collections` readable (correctness, not
  per-source read isolation).
- `DeleteSource` gains a cache side effect — must be reviewable as part of the
  source lifecycle, and must be reference-aware (a source whose root is shared
  with another registered source keeps the caches).
- ADR 0011's collections section is superseded; its venv, tofu-provider,
  tofu-modules, and system-package decisions are untouched.

## Addendum — 2026-10-06: local-directory artifacts are read-granted

A requirements file may name a collection on the local disk with `type: dir`
(or `type: file`), pointing at a path outside the project root — typically a
sibling checkout (`… - name: …/ansible-unifi-network` /
`type: dir`). The resolver sandbox granted only the project root, so
`ansible-galaxy` opening the artifact's `galaxy.yml` was refused with `EACCES`
and the deps step failed closed:

```
PermissionError: [Errno 13] Permission denied: b'/…/ansible-unifi-network/galaxy.yml'
```

`deps.DiscoverGalaxy` now reports the existing local artifact paths the
requirements file names, and the sandboxed resolve adds exactly those to the
profile's read grants on top of the project root. A galaxy-server, git, or url
entry is dialed rather than read and adds no grant; a path that does not exist
adds none, because a Landlock grant attaches only to a real path. The profile
guard is unchanged and still refuses any grant that would reach the data dir or
its keys. This is a read-only widening drawn from source content — the same
shape as the run sandbox granting the inventory and extra-vars paths an argv
names — and is recorded as a residual in
[the threat model](../security/threat-model.md).
