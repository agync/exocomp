# MinIO assessment (2026-08-21)

Role in this project, before the fixture swap below: **test fixture only** — an unmodified
upstream container in `internal/backup/s3_test.go` and `test/e2e/m1-gate.sh`. Shipped
code has zero MinIO references; the S3 leg is restic's generic `s3:` backend plus plain
SigV4 requests.

**Update:** the fixture is now SeaweedFS; see Decision below. This page stays as
the license/health record that motivated the swap.

## License

- Server: AGPL-3.0 since 2021, unchanged. `minio-go` SDK (unused here): Apache-2.0.
- AGPL as a test fixture: running an unmodified image and talking to it only over the
  S3 HTTP API imposes no obligation on this repository — the network-copyleft clause
  attaches to conveying or modifying the server, neither of which happens here. This
  reading flips if a MinIO binary or image is ever redistributed or patched by this
  project. (Assessment, not legal advice.)

## Health — the actual risk

Community edition is dead: admin features stripped to the paid AIStor tier
(Feb–May 2025), prebuilt images halted (Oct 2025), maintenance mode (Dec 2025),
repository archived (2026-04). License risk: none. Abandonment risk: total.

## Decision

- **Done: the fixture is SeaweedFS** (Apache-2.0, active,
  `docker.io/chrislusf/seaweedfs`, `weed server -s3`) in `internal/backup/s3_test.go` and
  `test/e2e/m1-gate.sh`. Verified against the real restic leg: init, backup, restore,
  and this repository's own hand-rolled SigV4 client (`internal/backup/s3.go`, used for
  `keyring.age`/`RECOVERY.md` at the bucket root, outside restic's own key space) all
  pass unchanged. The full `test/e2e/m1-gate.sh` gate passes end to end against it.
- Two things SeaweedFS needed that MinIO did not, both fixture-only and neither a
  restic/SigV4 incompatibility:
  - **Real credentials, not anonymous.** SeaweedFS accepts an unsigned request with no
    `-s3.config` at all, but rejects a *signed* one outright
    ("`Signed request requires setting up SeaweedFS S3 authentication`") — and restic
    always signs. The fixture mounts a small `-s3.config` identity file naming a
    throwaway access key/secret; anonymous mode is not viable for a restic fixture.
  - **World-readable config, not 0600.** Rootless podman's user-namespace UID remap
    means the container's own user is neither the bind-mounted config file's nor its
    directory's owner; `0600` (this project's usual default for anything holding
    material) fails "permission denied" reading it inside the container. The file holds
    only the fixture's dummy credential, never a real secret, so `0644`/`0755` is fine
    here specifically.
- Operator docs no longer suggest MinIO as a fresh deployment choice; existing installs
  keep working (S3 data plane intact) — the S3 leg was always restic's generic `s3:`
  backend, never MinIO-specific.
- Not relevant to state locking: ADR 0004 moved locking into SQLite precisely
  because S3-conditional-write support is inconsistent across stores; no alternative
  fixture needs `If-None-Match` on PUT.
