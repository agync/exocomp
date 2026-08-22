# Architecture decision records

One decision per file, each in Status / Context / Decision / Consequences form. An ADR is
amended by a later ADR that supersedes it, never edited to change its meaning; corrections
of fact land in [the evolution log](../log.md).

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-thin-core-architecture.md) | Thin-core architecture — one Go binary owning state, sequencing, keys, and recovery; everything else a pinned subprocess | Accepted |
| [0002](0002-project-name-exocomp.md) | Project name **exocomp**, after the Control D `ctrld` collision ruled out the working name | Accepted |
| [0003](0003-secrets-envelope.md) | Secrets envelope: age recipients → DEK → HKDF subkeys, with a recovery kit that closes the credential loop | Accepted |
| [0004](0004-opentofu-http-state-backend.md) | OpenTofu HTTP state backend implemented in SQLite, sidestepping the S3 conditional-write gap | Accepted |
| [0005](0005-job-sandboxing-nono.md) | Job sandboxing via nono's `local-sandboxed` tier; jobs are denied every powerful local socket | Accepted |
| [0006](0006-uv-toolchain-escape-hatch.md) | uv build-time Python toolchain plus a doctor-gated operator escape hatch in the data volume | Accepted |

Decisions recorded elsewhere because they are shape rather than choice: the twelve
critique-driven changes are in [design/amendments.md](../design/amendments.md), and the
platform support tiers are in
[architecture/platform-contract.md](../architecture/platform-contract.md).
| [0007](0007-audit-chain-deferral.md) | Audit hash chain deferred; revival requires an off-host-anchored head |
| [0008](0008-step-ledger-over-dbos.md) | Multi-step choreography via a step ledger on the job store; DBOS deferred |
| [0009](0009-lan-access-tokens.md) | LAN listeners require locally-minted bearer tokens; TLS by default |
| [0010](0010-source-and-credential-model.md) | Git-centric source model; deploy/fleet keys; TF state-only vs run-in-server |
| [0011](0011-project-dependency-resolution.md) | Project deps from root requirements.txt/pyproject.toml, uv-resolved into hash-gated caches; uv ships at runtime |
| [0012](0012-litestream.md) | Litestream as an optional fast-RPO sidecar replicating exocomp.db's WAL; restic stays authoritative; unencrypted-remote refused without ack; sidecar not embedded |
