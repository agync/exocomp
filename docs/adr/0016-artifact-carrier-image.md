# ADR 0016 — exocomp ships as an artifact-carrier image

## Status

Accepted · 2026-09-17 — amends [ADR 0006](0006-uv-toolchain-escape-hatch.md) (the
control plane may install uv and manage its own fallback ansible toolchain) and
[ADR 0011](0011-project-dependency-resolution.md) (ansible-core stops being
forced into every source venv).

## Context

exocomp is one container. An operator runs the stock image, but also wants to add
what the base cannot carry: a system package a playbook shells out to, a native
library a Python module needs, a different ansible-core. Two constraints shape how
that is served:

- The base must work on its own — no build required to run exocomp.
- Whatever an operator adds must not break the properties the project is bought
  for: the one queue, the sandbox, and a recovery path that depends on no registry
  (invariant 7).

ADR 0006 pinned the Python toolchain to the image and made uv, in the data volume,
an operator-only escape hatch; ADR 0011 had every source venv carry its own
`ansible-core`, forced to the image pin. Both predate treating the container image
itself as a distributable artifact. uv's own Docker guide shows the shape adopted
here: its image exists to hand out two static binaries (`/uv`, `/uvx`) that a
`COPY --from` drops into any base.

## Decision

The base image is both a runnable control plane and an artifact carrier, and
exocomp manages its own fallback toolchain with uv.

### The base image

- `ghcr.io/abn/exocomp` is published multi-arch (manifest list) and digest-pinned.
  It is self-contained: `exocompd`, the pinned subprocess tools, the baked
  Python/ansible layer, uv and uvx.
- uv is taken with the documented copy pattern —
  `COPY --from=<uv image> /uv /uvx /bin/` — with the uv image pinned by digest,
  not tag.
- Install may use a tag or the digest; recovery never reads a registry.

### uv is bootstrapped when absent

exocomp installs a pinned uv (version + sha256, checked as `tool-digests.txt` is)
into the data volume when no `uv` is on `PATH`, and uses that. Offline with no uv
present is a loud failure: the recovery path must have uv already present, never
fetch it. This amends ADR 0006's "the control plane never invokes uv" — uv is a
runtime dependency the control plane may satisfy itself.

### The Python version follows ansible-core, and is frozen

The image stops pinning an arbitrary interpreter. uv is given a constraint derived
from the ansible pin, and the exact resolved interpreter is frozen into
`MANIFEST.json`. Offline runs and the per-source venv path use the frozen
interpreter. uv does not select a Python from a dependency's `Requires-Python` on
its own, so the constraint is explicit rather than implied.

### ansible-core is a command, not a project dependency

- exocomp manages a fallback ansible toolchain with
  `uv tool install ansible-core==<MANIFEST pin> --with ansible-runner`, pinned and
  offline-aware, its bin on `PATH`.
- A per-source venv carries only the project's declared Python libraries. It is
  not given a forced `ansible-core`: the run's ansible is the PATH-resolved
  fallback, and the project's libraries reach it through `PYTHONPATH` set to the
  venv's `site-packages`.
- A project that declares `ansible-core` (or the `ansible` meta-package) installs
  it into its venv; the venv's `bin` is prepended to `PATH`, so the project's
  ansible shadows the fallback.
- The venv is created with the same frozen interpreter as the fallback, so the
  `site-packages` bridge is ABI-compatible.
- `ansible_python_interpreter` is target-side only and does not substitute for the
  `PYTHONPATH` bridge.
- This retires the forced per-venv pin (`internal/deps/venv.go`'s `toolchainPins`)
  and the `ansible`-meta-package conflict wrapper that existed only to explain it.

### Scope

The base image is supported and tested. Consuming its artifacts with `COPY --from`
into a foreign base is documented but unsupported and untested for now; the
deferral is recorded so it is a decision, not an omission.

### Disaster recovery

A container image an operator runs is captured into the recovery set as an archive
(`podman save`), because a local image tag lives in the store of the host that can
fail. A rebuild path additionally needs its source base images present. A registry
tag may be used to install, never to recover.

## Consequences

- A stock install's DR pin is the base image; a derived image is the operator's to
  capture.
- One ansible toolchain replaces one per source venv: smaller caches, no duplicated
  core, and no resolution conflict between a project pin and the image pin.
- `PYTHONPATH` joins the run environment, so the sandbox must grant read on the
  venv's `site-packages` dir; without it the bridge silently fails under the
  `enforcing` tier.
- uv's presence is self-healed, so a host or container without uv is a supported
  online state and an unsupported offline one.
- Determinism shifts from a `PYTHON_VERSION` input pin to a frozen resolved
  interpreter in `MANIFEST.json`.
- A system package added in a derived image needs a nono grant for a job to reach
  it; `doctor` must name the addition and the grant.
- BYO-runner stays deferred: nothing here builds images inside the daemon or
  mounts a podman socket.

Operator-facing detail lives in
[operations/containers.md](../operations/containers.md).
