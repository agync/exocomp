---
type: Guide
title: "Containers"
description: "The base image and what it carries, running it as shipped, inheriting to add system packages or Python dependencies, offline and multi-arch builds, and capturing an operator image for recovery."
status: stable
tags: [operations, containers, packaging]
---

# Containers

exocomp ships as one container. This page covers running it, extending it by
inheriting from it, and recovering it. The decisions behind it are
[Decision 0016](../decisions/0016-artifact-carrier-image.md); the recovery chain it plugs
into is [disaster recovery](disaster-recovery.md).

## Status

| Piece | State |
|---|---|
| Base image builds and runs (`make image`), `doctor` green | **shipped** |
| Inheriting from the base (`FROM exocomp`) | **supported** |
| Published `ghcr.io/agync/exocomp` (multi-arch, digest) | **shipped** (`.github/workflows/publish.yml`) |
| uv self-bootstrap; PATH-delegated ansible with `PYTHONPATH` | decided in Decision 0016, lands with the packaging work |
| `COPY --from` into a foreign base | **deferred** (Decision 0016, Scope) |

The units reference the published ref. To run a local build instead, substitute
`localhost/exocomp:dev` (`make image`) for the image name below.

## What the base image carries

- `exocompd` (the single binary and CLI) and the pinned subprocess tools: `tofu`,
  `restic`, `age`, `sops`, `nono`, plus `git` and `openssh-clients`.
- The baked Python + `ansible-core` toolchain, and `uv`/`uvx` pinned by digest
  ([bill of materials](../sbom/bill-of-materials.md)).
- Contract to preserve when inheriting: `USER 1000:1000`, data volume
  `/var/lib/exocomp`, `XDG_DATA_HOME=/var/lib`, the toolchain on `PATH`,
  `ENTRYPOINT`/`CMD` unchanged.

## Run it as shipped

```
make image
podman run --rm localhost/exocomp:dev doctor
```

Under systemd use the Quadlet unit; under Docker use the compose file
([platform contract](../architecture/platform-contract.md)). Both mount
`/var/lib/exocomp`; that volume is the only state, and it is what every backup
leg copies.

Two Quadlet constraints the unit keeps but no longer spells out in comments: it
sets **no `Tmpfs=`** on the data dir — under the rootless `UserNS=keep-id`
posture podman rejects one ("the 'noswap' option is only allowed with rootful
tmpfs mounts") and the container will not start; staged secret material lives in
`/dev/shm`, the RAM-backed tmpfs every rootless container has anyway. And it sets
**no `[Service] Type=`** — the generated unit is `Type=notify` by default and
conmon supplies `READY=1`, so it works without `exocompd` calling `sd_notify`.

Two Quadlet constraints the unit keeps but no longer spells out in comments: it
sets **no `Tmpfs=`** on the data dir — under the rootless `UserNS=keep-id`
posture podman rejects one ("the 'noswap' option is only allowed with rootful
tmpfs mounts") and the container will not start; staged secret material lives in
`/dev/shm`, the RAM-backed tmpfs every rootless container has anyway. And it sets
**no `[Service] Type=`** — the generated unit is `Type=notify` by default and
conmon supplies `READY=1`, so it works without `exocompd` calling `sd_notify`.

## Add a system package

A playbook that shells out to a binary the base does not carry gets it by
inheriting:

```dockerfile
FROM ghcr.io/agync/exocomp:0.1.0
USER root
RUN dnf install -y --setopt=install_weak_deps=0 rsync sshpass \
    && dnf clean all
USER 1000:1000
```

Version-pin the packages and end on `USER 1000:1000`. Two things to know: a
system binary a job runs must be reachable under the job sandbox, so a
nono grant is needed and `exocomp doctor` reports the addition; and this is the
one seam Decision 0011 leaves out of runtime scope, which is why it is an image
change and not a manifest change.

When running a custom derived image under systemd, update both `Image=` and
`Environment=EXOCOMP_IMAGE=` in `~/.config/containers/systemd/exocomp.container`
so the unit executes your image tag and records it for disaster recovery (Decision 0016).

## Add Python dependencies

Prefer the declared-manifest path. A project's root `requirements.txt` or
`pyproject.toml` (PEP 621 / dependency-groups) is resolved by uv into a
per-source, hash-gated venv — offline after the first resolve
([Decision 0011](../decisions/0011-project-dependency-resolution.md)). A source that
declares nothing stays on the shared toolchain.

To change the shared toolchain itself, the operator escape hatch applies uv at a
shell into the data volume, so the switch persists, rides in backups, and is
visible to `doctor` ([Decision 0006](../decisions/0006-uv-toolchain-escape-hatch.md)):

```
uv tool install --python <interpreter> ansible-core==<version> --with <module-dep>
```

`doctor` prints the active manifest path and marks each tool whose resolved version is off
its `MANIFEST.json` pin as a `warn` (`restic 0.19.1 — pinned 0.19.0`): the switch is a
supported state made visible, not a fault.

Do not bake Python libraries into an image you only use for one project; the
manifest path is the supported one. Baking is for a dependency every run needs.

## Pin a different ansible-core

Today the image pin moves through the Decision 0006 escape hatch or an image rebuild;
a per-source venv does not override it. Decision 0016 changes this: the fallback
toolchain is exocomp-managed and a project that declares `ansible-core` in its
manifest shadows it. Landed behavior follows that record.

## Offline and air-gapped

A resolve is hash-gated, so a stable manifest never dials out. For a host with no
network, vendor collections and providers first — see
[offline caches](offline-caches.md). An image built for an air-gapped host must
carry its base images and the wheelhouse; `uv` runs with `--offline`.

## Multi-arch

The base is published as a manifest list (`amd64`, `arm64`). Publish a derived
image for the same platforms, and pin `FROM` by `@sha256:` rather than a tag so a
rebuild does not silently drift.

`.github/workflows/publish.yml` builds that list. Each architecture builds on its
own runner (`ubuntu-24.04`, `ubuntu-24.04-arm`) rather than under emulation, so no
qemu is involved, and the build passes `TARGETARCH` explicitly because the tool
downloads are arch-specific. Per-arch tags are pushed as `:build-amd64` and
`:build-arm64`, then a manifest job assembles them into the list and pushes
`:latest`, plus the version tag when the run came from a tag. It runs on pushes to
`main`, on `v*` tags, and on `workflow_dispatch`.

The registry login needs no secret: `ghcr.io/agync/exocomp` is the repository
owner's namespace, which is the one a workflow's built-in `GITHUB_TOKEN` can write.
Set the `IMAGE_NAME` repository variable to publish elsewhere, such as a fork's own
namespace.

## Disaster recovery of a custom image

A local image tag lives in the store of the host that can fail, so it is not a
backup. Capture the image you run into the recovery set:

```
podman save -o exocomp-image.tar <image-ref>
```

and record the image ref and every source base digest in `RECOVERY.md`
([disaster recovery](disaster-recovery.md)). A registry tag may be used to
install; recovery must not need it (invariant 7). `podman load -i FILE` (or
`docker load -i FILE` on the Docker tier) loads the captured archive with the
detected engine before the Quadlet units start. The unit's `EXOCOMP_IMAGE` is
what the daemon records, so the `image-ref:` line in `RECOVERY.md` and
`exocomp doctor` name what actually runs.

## Updating and rollback

Rebuild a derived image when its base digest moves, then roll the unit forward
with the normal upgrade path ([upgrade and rollback](upgrade.md)). An old image
tag is a rollback target; keep the previous one until the new one has run a
backup and a restore drill.
