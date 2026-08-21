# Bill of materials

Indicative pins as of 2026-08. **Exact versions are recorded per build in `MANIFEST.json`
and enforced by `exocomp doctor`**, which refuses to run a job whose tool version is
outside a tested range rather than failing later with a parse error.

## The license boundary

GPL components — `ansible-core`, `git` — are **subprocesses, never linked**, so
`exocompd`'s own license stays unencumbered. This is a structural rule, not a preference:
it is the same reason Dagu (GPL-3.0, and otherwise exactly the right shape at 16 MB with no
database) was rejected, because embedding it would have forced the whole binary's license.

Anything that would have to be linked must be permissively licensed. Anything GPL crosses
a process boundary or does not ship.

## Layer 1 — compiled into `exocompd` (one static Go binary)

| Component | Pin | License | Role |
|---|---|---|---|
| Go toolchain | 1.26+ | BSD-3 | `CGO_ENABLED=0` static builds, amd64 + arm64 |
| `modernc.org/sqlite` | 1.57.x | BSD-3 | embedded DB, pure Go, no CGO; `exocomp.db`, WAL |
| `filippo.io/age` | 1.3.x | BSD-3 | envelope crypto as a library (recipients → DEK) |
| Go stdlib `crypto` (GCM, `crypto/hkdf`) | 1.26 stdlib | BSD-3 | secret encryption and subkey derivation — zero novel crypto |
| `robfig/cron/v3` | 3.0.x | MIT | schedules, rebuilt from the DB on boot |
| `google/cel-go` | 0.2x | Apache-2.0 | sandboxed, non-Turing-complete event rules |
| `containrrr/shoutrrr` | 0.8.x | MIT | notification fan-out (ntfy, gotify, SMTP, webhook) as a library |
| `charm.land/bubbletea/v2` + `bubbles/v2` + `lipgloss/v2` | v2.0.x / v2.2.x / v2.0.x | MIT | TUI — the priority-1 interface. Not the `charmbracelet/*` v1 imports — Charm's tooling moved to the `charm.land` module path for the v2 line this project is on |
| `charm.land/wish/v2` | 2.0.x | MIT | TUI over SSH, `exocomp ssh-serve` (`cmd/exocomp/ssh.go`). Not `charmbracelet/wish` v1.x — that major still pairs with Bubble Tea v1; `charm.land/wish/v2`'s `bubbletea` middleware is the release that targets Bubble Tea v2, which this project is on |
| `a-h/templ` + htmx | 0.3.x / 2.x | MIT / 0BSD | server-rendered web UI via `go:embed`, no Node build |
| `spf13/cobra` | 1.x | Apache-2.0 | CLI surface |
| *(deferred)* Wails | v2 | MIT | desktop wrapper of the web UI |

Binary target ~20 MB; idle RSS ≤ 60 MB, with CI failing above 80 MB.

## Layer 2 — pinned subprocess tools shipped in the image

| Tool | Pin | License | Role | Interface contract |
|---|---|---|---|---|
| uv | 0.8.x | Apache-2.0/MIT | build: `uv tool install` of the pinned Ansible toolchain, constraints-pinned and frozen into `MANIFEST.json`. Runtime: operator-only escape hatch, doctor-gated, never invoked by `exocompd` ([ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md)) | build stage + operator shell |
| ansible-core (+ Python via uv) | 2.1x / 3.12+ | GPL-3.0+ / PSF | playbook execution | M1: direct `ansible-playbook` subprocess, stdout line-scanned into events; exit codes gate success |
| ansible-runner | 2.4.x | Apache-2.0 | **not used.** M1 substituted a direct `ansible-playbook` driver — exit-code correctness, line-scanned output — and M2 confirmed staying direct rather than adopting ansible-runner's per-task JSON events and artifact capture. A JSON task-tree callback plugin over the direct driver is backlogged v1.1 | closed, v1.1 backlog |
| vendored Galaxy collections (`community.sops`, …) | `requirements.yml` lock | GPL-3.0 (mostly) | playbook deps, offline-restorable | data volume cache, in every backup |
| OpenTofu | 1.12.x | MPL-2.0 | plan/apply/drift; native state encryption | `-json` UI stream; HTTP backend → `exocompd` |
| restic | 0.19.x (sha256-pinned) | BSD-2 | encrypted backup engine, all three legs | `--json`; exit codes only for correctness |
| sops | 3.x | MPL-2.0 | repo-file secret ops, identical behaviour to the operator CLI | shelled out |
| age (CLI) | 1.2.x | BSD-3 | repo-file ops and the printed-card manual recovery path | shelled out |
| git | 2.4x | GPL-2.0 | project sources; bundle and push for the GitHub backup leg | plumbing only |
| nono | ≥ 0.55.0 (sha256-pinned) | Apache-2.0 | kernel sandbox around job subprocesses, the `local-sandboxed` tier and Linux default ([ADR 0005](../adr/0005-job-sandboxing-nono.md)) | `nono run --profile job-<kind>`; doctor probes the Landlock ABI |
| Litestream *(optional sidecar)* | 0.5.x | Apache-2.0 | continuous WAL → S3, seconds-level RPO | own process, config generated from the DB |

Image target ≤ 800 MB measured in CI, with a working target of ~500 MB. The base is a slim
glibc image — the Python requirement rules out distroless-static.

## Layer 3 — deployment substrate (host-provided)

| Component | License | Role |
|---|---|---|
| Podman + Quadlet (systemd) | Apache-2.0 | primary deployment: rootless, `MemoryMax=1.5G`, journald |
| docker-compose file | — | fallback for Docker-only hosts, supported at a loudly degraded tier |
| systemd-creds + TPM2 *(optional)* | LGPL/GPL (host) | TPM-sealed host age identity |
| age-plugin-yubikey *(optional)* | MIT/Apache-2.0 | hardware recipient in the keyring |
| `install.sh` | project | separately versioned bootstrap/restore/rollback — never depends on `exocompd` |

## Layer 4 — operator-supplied services (composed, not shipped)

| Service | License | Role | Required? |
|---|---|---|---|
| ntfy (self-hosted or token-authed) | Apache-2.0/GPL-2.0 | approval action buttons, notifications | yes — the approvals UX |
| healthchecks (self-hosted or hosted) | BSD-3 | dead-man's switch; ping URL on the recovery kit | yes — backup honesty |
| Prometheus + Alertmanager (existing) | Apache-2.0 | event source in; `/metrics` and seeded rules out | recommended |
| NAS share (SMB/NFS) | — | restic leg 1 | ≥ 1 of the |
| S3-compatible store (Garage/SeaweedFS/B2/R2 (MinIO CE archived 2026-04, see the MinIO assessment)) | varies | restic leg 2; optional Litestream target | three legs |
| private GitHub/Gitea repo | — | leg 3: age-encrypted orphan-commit archives | required |
| Git forge for ops repos | — | playbook and OpenTofu sources, bundled into every backup | yes |
| Tailscale/WireGuard *(optional)* | BSD-3 | the specified remote-access path for approvals; Caddy (Apache-2.0) is the fallback | recommended |

## Layer 5 — budget rollup

A number in a test is a constraint; a number in a README is a wish. Every row below is
enforced by something.

| Budget | Value | Enforced by |
|---|---|---|
| Image size | ≤ 800 MB | CI |
| `exocompd` idle RSS | ≤ 60 MB, fail above 80 MB | CI |
| Container memory | `MemoryMax=1.5G` | the Quadlet unit |
| Concurrent jobs | 2 by default, memory-derived | queue config |
| Target hardware | 4 GB N100 / Raspberry Pi 4, amd64 + arm64 | reference benchmark in CI |

The concurrency default is derived rather than chosen: Ansible forks and OpenTofu providers
are the real memory consumers, not the daemon. CI additionally measures peak RSS of a
reference Ansible-plus-OpenTofu job, so the budget covers the whole container rather than
just the binary.
