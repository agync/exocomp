# MinIO assessment

The S3 leg is restic's generic `s3:` backend plus plain SigV4 requests; shipped code
carries no MinIO or SeaweedFS-specific logic. The test fixture that exercises this leg is
SeaweedFS (Apache-2.0, active, `docker.io/chrislusf/seaweedfs`, `weed server -s3`), used in
`internal/backup/s3_test.go` and `test/e2e/m1-gate.sh`. This page records the license and
health assessment that ruled MinIO out as that fixture.

## Why not MinIO

- **License.** Server: AGPL-3.0 since 2021. Running an unmodified image and talking to it
  only over the S3 HTTP API imposes no obligation on this repository — the network-copyleft
  clause attaches to conveying or modifying the server, neither of which happens here. That
  reading flips if a MinIO binary or image is ever redistributed or patched by this project.
  (Assessment, not legal advice.) The `minio-go` SDK, unused here, is Apache-2.0.
- **Health.** MinIO Community Edition is dead: admin features stripped to the paid AIStor
  tier (Feb–May 2025), prebuilt images halted (Oct 2025), maintenance mode (Dec 2025),
  repository archived (2026-04). License risk is none; abandonment risk is total.

## The SeaweedFS fixture

Verified against the real restic leg: init, backup, restore, and this repository's own
hand-rolled SigV4 client (`internal/backup/s3.go`, used for `keyring.age`/`RECOVERY.md` at
the bucket root, outside restic's own key space) all pass unchanged. The full
`test/e2e/m1-gate.sh` gate passes end to end against it.

SeaweedFS needs two things MinIO did not, both fixture-only — neither is a restic/SigV4
incompatibility:

- **Real credentials, not anonymous.** SeaweedFS accepts an unsigned request with no
  `-s3.config` at all, but rejects a *signed* one outright ("`Signed request requires
  setting up SeaweedFS S3 authentication`") — and restic always signs. The fixture mounts a
  small `-s3.config` identity file naming a throwaway access key/secret; anonymous mode is
  not viable for a restic fixture.
- **World-readable config, not 0600.** Rootless podman's user-namespace UID remap means the
  container's own user is neither the bind-mounted config file's nor its directory's owner;
  `0600` (this project's usual default for anything holding material) fails "permission
  denied" reading it inside the container. The file holds only the fixture's dummy
  credential, never a real secret, so `0644`/`0755` is fine here specifically.

## Scope

Operator docs do not suggest MinIO as a fresh deployment choice. Existing installs using
MinIO keep working: the S3 leg is restic's generic `s3:` backend, never MinIO-specific.
State locking is unaffected: [ADR 0004](../adr/0004-opentofu-http-state-backend.md)
moved locking into SQLite because S3 conditional-write support is inconsistent across
stores, so no fixture needs `If-None-Match` on PUT.
