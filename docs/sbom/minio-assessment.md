# MinIO assessment (2026-08-21)

Role in this project: **test fixture only** — an unmodified upstream container in
`internal/backup/s3_test.go` and `test/e2e/m1-gate.sh`. Shipped code has zero MinIO
references; the S3 leg is restic's generic `s3:` backend plus plain SigV4 requests.

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

- Keep the current fixture short-term; the cached image keeps working and nothing
  ships it.
- **Backlog (M2b/M3): switch the fixture to SeaweedFS** (Apache-2.0, active, true
  one-liner `weed server -s3`) in both test files; verify its SigV4 + multipart
  behavior against the restic leg before swapping.
- Operator docs must stop suggesting MinIO as a fresh deployment choice; existing
  installs keep working (S3 data plane intact).
- Not relevant to state locking: ADR 0004 moved locking into SQLite precisely
  because S3-conditional-write support is inconsistent across stores; no alternative
  fixture needs `If-None-Match` on PUT.
