# Architecture decision records

One decision per file, each in Status / Context / Decision / Consequences form. A record is
amended by a later record that supersedes it, never edited to change its meaning; corrections
of fact land in [the evolution log](../log.md).

| Record | Decision | Status |
|---|---|---|
| [0001](0001-thin-core-architecture.md) | Thin-core architecture — one Go binary owning state, sequencing, keys, and recovery; everything else a pinned subprocess | Accepted |
| [0002](0002-project-name-exocomp.md) | Project name **exocomp**, after the Control D `ctrld` collision ruled out the working name | Accepted |
| [0003](0003-secrets-envelope.md) | Secrets envelope: age recipients → DEK → HKDF subkeys, with a recovery kit that closes the credential loop | Accepted |
| [0004](0004-opentofu-http-state-backend.md) | OpenTofu HTTP state backend implemented in SQLite, sidestepping the S3 conditional-write gap | Accepted |
| [0005](0005-job-sandboxing-nono.md) | Job sandboxing via nono's `enforcing` tier; jobs are denied every powerful local socket | Accepted |
| [0006](0006-uv-toolchain-escape-hatch.md) | uv build-time Python toolchain plus a doctor-gated operator escape hatch in the data volume | Accepted |
| [0007](0007-audit-chain-deferral.md) | Audit hash chain deferred; revival requires an off-host-anchored head | Accepted |
| [0008](0008-step-ledger-over-dbos.md) | Multi-step choreography via a step ledger on the job store; DBOS deferred | Accepted |
| [0009](0009-lan-access-tokens.md) | LAN listeners require locally-minted bearer tokens; TLS by default | Accepted |
| [0010](0010-source-and-credential-model.md) | Git-centric source model; deploy/fleet keys; state-only vs run-in-server workspaces | Accepted |
| [0011](0011-project-dependency-resolution.md) | Project deps from root requirements.txt/pyproject.toml, uv-resolved into hash-gated caches; uv ships at runtime | Accepted |
| [0012](0012-litestream.md) | Litestream as an optional fast-RPO sidecar replicating exocomp.db's WAL; restic stays authoritative; unencrypted-remote refused without ack; sidecar not embedded, and the image ships the binary | Accepted |
| [0013](0013-openbao-transit-seal.md) | OpenBao Transit as an optional additive seal for the DEK (auto-unseal); keyring.age recovery preserved; unreachable is an explicit error, never silent fallback | Accepted |
| [0014](0014-agent-facing-surface.md) | Agent-facing surface: one core, three thin faces (CLI --output json, HTTP /api/v1, MCP) sharing Decision 0009 auth, the one queue, and the internal/wire envelope contract | Accepted |
| [0015](0015-per-source-collections.md) | Ansible collections go per-source: content-hash-keyed cache dir, resolved at claim time, GC'd with the source lifecycle; Decision 0011's shared collections decision superseded | Accepted |
| [0016](0016-artifact-carrier-image.md) | exocomp ships as a self-contained base image that is also an artifact carrier; uv is bootstrapped when absent, ansible is PATH-delegated with `PYTHONPATH`, and an operator's image is captured for recovery | Accepted |
| [0017](0017-shared-probe-vocabulary.md) | One `internal/probe` vocabulary for checks and gates; checks gain dns/tls/metric; the SSRF guard is per-face and Location is opt-in per kind | Accepted |
| [0018](0018-aggregate-check-events.md) | Opt-in aggregate check events carrying the present host set, with `--limit-hosts` expansion governed by the trusted-source line | Superseded by [0019](0019-aggregate-check-events-removed.md) |
| [0019](0019-aggregate-check-events-removed.md) | Aggregate check events, `event.hosts`, and `--limit-hosts` removed; per-host events are simpler, and migration 0041 stays as inert schema | Accepted |
| [0020](0020-reload-restart-and-recovery.md) | Reload first, restart after drain, recover interrupted jobs; schedules reload without a restart | Accepted |
| [0021](0021-host-request-polling.md) | Keep the host scope write-only; the request response itself is the polling surface | Accepted |
| [0022](0022-pre-1.0-release-line.md) | Withdraw the v1.0 label; the release line is pre-1.0 from `v0.1.0`, and release-please owns later versions | Accepted |
| [0023](0023-source-egress-defaults.md) | An operator-set default egress set per source, unioned into every ansible run that names it at queue admission; repo content can never declare a port | Accepted |

Some decisions are recorded elsewhere because they are shape rather than choice: the platform
support tiers are in
[architecture/platform-contract.md](../architecture/platform-contract.md).
