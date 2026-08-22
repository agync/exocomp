# ADR 0006 — uv build-time toolchain with an operator escape hatch

## Status

Accepted · 2026-08-21

## Context

`ansible-core` is Python, so the image carries a Python layer whatever else is true. That
layer needs three properties at once, and they pull against each other:

- **Determinism** — a restore must produce the same toolchain that produced the backup.
- **A version switch for the operator** — homelabs run playbooks written against a
  particular ansible-core, and rebuilding the image to change it is not an acceptable
  answer.
- **No registry on the DR path** — an on-demand installer anywhere near the job path
  reintroduces the network dependency the offline-cache work exists to remove.

## Decision

Build the Python layer with **uv**: `uv python install` (standalone CPython) plus one keyed
tool install — `uv tool install ansible-core==X --with ansible-runner --with <module deps>`
— pinned by a committed constraints file, with the resolved set frozen into
`MANIFEST.json` alongside the tool pins. (`uv tool install` does not consume a project
`uv.lock`; the constraints file plus the frozen manifest give the same determinism.)
Module-side dependencies (netaddr, jmespath, pywinrm, …) live inside the tool venv via
`--with`.

**uv ships in the image, and `UV_TOOL_DIR` / `UV_CACHE_DIR` point into the data volume.**
That single placement turns "uv happens to be present" into a designed operator escape
hatch: a human can `uv tool install ansible-core==Y` at a shell and the switch

1. persists across container restarts,
2. rides inside every backup — so a restore comes back with the switched version, no
   registry on the DR path, and
3. is visible: `exocomp doctor` reports which binary each shim resolves to and diffs it
   against `MANIFEST.json`.

Boundary rules keep the hatch from becoming a hole:

- **The control plane never invokes uv.** No `uvx`, no on-demand installs on the job path.
  The hatch is operator-initiated, at a shell, on purpose.
- **Out-of-range versions are loud.** The golden-file parsing corpus is valid only for
  tested version ranges; `doctor` flags a switched toolchain and jobs refuse to run against
  it unless the project carries an explicit, acknowledged override.
- **DR-path runbooks — backup, restore drill, host onboarding — always execute the baked-in
  image pins**, never the volume toolchain: a fresh restore has the image before it has the
  volume. Enforced by the doctor, asserted by the drill.

## Consequences

- Per-project Python module dependencies may likewise be uv-installed into cached venvs
  under the same volume toolchain directory — backed up, offline-restorable, drill-covered.
  [ADR 0011](0011-project-dependency-resolution.md) refines the "control plane never invokes
  uv" boundary for exactly this case: exocomp MAY resolve a project's *declared* deps
  (root `requirements.txt`/`pyproject.toml`) via uv, hash-gated and off the DR path, while
  the ansible-core toolchain switch stays operator-initiated.
- The 800 MB image ceiling stands and the working target drops to ~500 MB.
- The escape hatch widens the tested surface: a switched toolchain is a supported *state*,
  not a supported *configuration*, and the doctor is what makes the difference visible.
- Toolchain state now lives in the data volume, so volume loss is toolchain loss. That is
  acceptable precisely because the DR path is pinned to the image instead.
