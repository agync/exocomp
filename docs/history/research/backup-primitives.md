# Backup Primitives for the Control Stack

Research note. Scope: encrypted backup of a small control-plane's own state — a SQLite
DB, YAML/TOML config, git repos (playbooks/IaC), and secrets — to a NAS (SMB/NFS),
S3-compatible object storage (Backblaze B2, R2, MinIO, etc.), and optionally a GitHub
repo. Evaluated against the project goals: single-binary/single-container, homelab
scale, trivial bootstrap/DR, "compose don't build."

Last checked: 2026-08-21. Version/date claims below are cited inline; treat anything
without a citation as inference, not fact.

---

## Landscape table

| Tool | Language | License | Footprint | Single binary? | Health (2026) |
|---|---|---|---|---|---|
| **restic** | Go | BSD-2-Clause [[restic/LICENSE]](https://github.com/restic/restic/blob/master/LICENSE) | ~20-30MB static binary, no daemon | Yes | Active. v0.19.1 released 2026-07-05 [[restic blog]](https://restic.net/blog/2026-07-05/restic-0.19.1-released/) |
| **BorgBackup / borgmatic** | Python | BSD-3-Clause (borg) | Python + deps installed via pip/pipx; borgmatic is a thin YAML wrapper around borg | No (Python runtime + venv, or PyInstaller bundle) | Active. Borg 1.4.5 stable (2026-07-19); Borg2 still beta (2.0.0b22, 2026-07-22) [[borgbackup.org/releases]](https://www.borgbackup.org/releases/) |
| **Kopia** | Go | Apache-2.0 [[kopia/LICENSE]](https://github.com/kopia/kopia/blob/master/LICENSE) | ~40-60MB binary; optional bundled web-UI/server adds size | Yes (CLI); GUI is a separate Electron app | Active development, but the embeddable `repo` library API is explicitly marked pre-1.0 and unstable [[pkg.go.dev/kopia/repo]](https://pkg.go.dev/github.com/kopia/kopia/repo) |
| **rustic** (rustic-rs) | Rust | Apache-2.0 / MIT dual | Single static binary, comparable to restic | Yes | Restic-repo-format compatible, but explicitly **beta, "not recommended for production backups yet," missing regression tests** as of mid-2026 [[rustic docs]](https://docs.rs/crate/rustic-rs/latest) |
| **rclone** (+ `crypt`) | Go | MIT [[rclone.org/licence]](https://rclone.org/licence/) | Single static binary | Yes | Very active, broad backend ecosystem; not a backup tool per se (no snapshots/dedup) |
| **Litestream** | Go | Apache-2.0 | Single static binary, runs as sidecar | Yes | Active. v0.5.16 released 2026-08-05 [[litestream releases]](https://github.com/benbjohnson/litestream/releases) |
| **LiteFS** | Go | Apache-2.0 | Single static binary + FUSE | Yes | Stable but **deprioritized by Fly.io**; LiteFS Cloud sunset Oct 2024, project frozen in pre-1.0 beta [[Fly.io alternatives note]](https://litestream.io/alternatives/) |
| **git-crypt** | C++ | GPL-3.0 | Tiny binary + git hooks | Yes (binary), but needs GPG or a symmetric key file | Mature, slow-moving. v0.8.0 released 2025-09-23; project itself says pre-1.0, API/format may still break [[AGWA/git-crypt]](https://github.com/AGWA/git-crypt) |
| **age + sops** | Go | BSD-3 (age) / MPL-2.0 (sops) | Two small static binaries | Yes (each) | Very active; sops is the de-facto GitOps secrets standard |
| **git-remote-gcrypt** | Shell | GPL-3.0 | Shell script, needs GPG/gpg-agent | No | Mature, low-velocity; niche but works |
| **git-remote-restic** | Go | project-specific (MIT-style) | Thin glue binary over restic | Yes | Small hobby project, low activity — verify before depending on it [[manwithafox/git-remote-restic]](https://github.com/manwithafox/git-remote-restic) |

---

## Per-option analysis

### restic

- **Encryption model**: Client-side AES-256-CTR + Poly1305 MAC, keys derived from a
  repository passphrase (scrypt) or a key file; every blob is encrypted and
  authenticated before it leaves the host. No plaintext ever touches the backend.
- **Targets**: Native backends for local/mounted paths (covers **NFS/SMB via a normal
  mount point** — no special code needed), SFTP, REST server, Backblaze B2 (native
  API), Amazon S3 and S3-compatible (MinIO, R2, Wasabi), Azure, GCS, and — via `rclone`
  as a backend — **any of rclone's 70+ providers**, which is how you'd also reach
  Google Drive, WebDAV, etc. [[restic + rclone guide]](https://botmonster.com/self-hosting/set-up-automatic-encrypted-backups-restic-rclone/) [[restic docs: preparing a repo]](https://restic.readthedocs.io/en/latest/030_preparing_a_new_repo.html)
- **Compression**: Added in v0.14 (2022), zstd, default "auto" [[restic 0.14 release]](https://restic.net/blog/2022-08-25/restic-0.14.0-released/) — older comparison articles claiming "restic has no compression" are stale.
- **Restore ergonomics**: `restic restore`, `restic mount` (FUSE browsing of any
  snapshot), `restic dump` for single-file extraction without a full restore. Simple
  and scriptable.
- **Verification**: `restic check` validates repository structure and (optionally)
  reads all data blobs (`--read-data`) to catch bitrot before you need it.
- **Footprint**: Single static Go binary, no daemon, low idle memory. Memory during
  backup scales with dataset size (large repos can spike during index rebuild), a
  known operational caveat but not relevant at homelab scale (MB-GB of state).
- **Embedding**: Deliberately **not designed as a library** — all backup logic sits
  under `internal/` specifically to keep the API private, and this is a known,
  long-standing, unresolved feature request [[restic#4406]](https://github.com/restic/restic/issues/4406) [[pkg.go.dev/restic/restic/internal]](https://pkg.go.dev/github.com/restic/restic/internal). Any Go control-plane must **shell out** to the `restic` binary and parse
  `--json` output; there is no supported `import "github.com/restic/restic/..."` path.
- **License**: BSD-2-Clause — maximally permissive, zero friction for bundling the
  binary into a container image.

### BorgBackup / borgmatic

- **Encryption model**: AES-256 (or ChaCha20-Poly1305 in Borg2) with HMAC, key can be
  stored in the repo (passphrase-protected), in `~/.config/borg`, or "repokey"/"keyfile"
  modes for detached key management.
- **Targets**: Local paths (so NFS/SMB mounts work) and **SSH** natively. **No native
  S3/B2/object-storage backend** — you need `rclone mount` (FUSE) as a bridge, or an
  SSH-speaking gateway, both extra moving parts.
- **Compression**: Best-in-class — LZ4/zlib/LZMA/zstd choices, generally the smallest
  repos of the three majors.
- **borgmatic**: A Python/YAML config wrapper that adds scheduling glue, pre/post
  hooks, retention policies, and monitoring pings (healthchecks.io, ntfy, Cronitor,
  PagerDuty) out of the box — directly useful for this project's "reactive
  actions"/alerting goal if Borg were chosen [[borgmatic vs restic]](https://dev.to/selfhostingsh/borgmatic-vs-restic-which-backup-tool-to-use-4bgo).
- **Restore/verification**: `borg extract`, `borg mount` (FUSE), `borg check`
  (structure + optional data integrity pass).
- **Footprint**: Python runtime dependency is the main friction for a "single
  container" goal — not heavy, but it's an interpreter + venv rather than a static
  binary, and PyInstaller-bundled distributions exist but aren't the primary
  distribution path.
- **License**: BSD-3-Clause.
- **Verdict for this project**: Best compression and most mature verification/repair
  tooling, but the missing native object-storage backend is a real gap against the
  "S3-compatible + Backblaze" requirement, and it's not a Go-embeddable or
  single-static-binary component.

### Kopia

- **Encryption model**: AES-256-GCM or ChaCha20-Poly1305, content-addressable storage,
  keys derived from a password; strong integrity guarantees via Merkle-tree-style
  content hashing.
- **Targets**: Native S3-compatible, B2, Azure, GCS, WebDAV, SFTP, local/NFS/SMB
  mounts, and a self-hostable Kopia repository server for multi-client setups.
- **Restore ergonomics**: CLI restore, a genuinely good **web UI** (bundled, runs
  locally) and a desktop Electron GUI (KopiaUI) — closest of this group to a
  Terraform-Cloud-esque "look at my backup runs in a browser" experience, at the cost
  of extra footprint if you ship the UI/server component.
- **Verification**: `kopia snapshot verify`, plus scheduled maintenance
  (compaction/GC) baked into the tool.
- **Embedding**: Better story than restic in principle — Kopia ships `repo`, `object`,
  and `content` as importable Go packages (`github.com/kopia/kopia/repo`, etc.)
  precisely so other Go programs can embed it [[pkg.go.dev/kopia/repo]](https://pkg.go.dev/github.com/kopia/kopia/repo). In practice the docs still warn the repository
  format and API are **not yet stable / not ready for general library use** as of the
  latest package docs — usable but pin the version and expect breakage across minor
  releases.
- **License**: Apache-2.0.
- **Verdict**: The most promising Go-native embedding path of the "big three," and the
  only one with a real bundled UI. If a future iteration of the control stack wants
  backup logic in-process rather than shelled out, Kopia's `repo` package is the one
  worth prototyping against — with the caveat that it's pre-1.0 and you should vendor/
  pin it tightly.

### rustic / rustic_core

- Restic-repository-format-compatible (a restic repo can be read/written by either
  tool, useful as a fallback/verification cross-check) [[rustic-rs/rustic]](https://github.com/rustic-rs/rustic).
- Split into `rustic` (CLI), `rustic_core` (embeddable library), `rustic-backend`
  (storage backends) — genuinely designed for embedding, which is attractive if the
  control-plane component doing backups is Rust rather than Go.
- **Explicitly not production-ready**: project docs state beta status and missing
  regression tests as of the latest release checked [[docs.rs/rustic-rs]](https://docs.rs/crate/rustic-rs/latest). Track it, don't depend on it yet for a system whose entire disaster-recovery story rests
  on the backup tool working correctly.

### rclone (+ crypt)

- Not a backup tool: no snapshots, no deduplication, no versioning by default — it's a
  sync/transport tool. Its role here is either (a) as **restic's backend transport**
  to reach providers restic doesn't support natively, or (b) as a **standalone
  encrypted mirror** of plain files (config, git repo checkouts) via the `crypt`
  remote.
- **`crypt` encryption model**: NaCl secretbox (XSalsa20-Poly1305) for file content;
  filenames encrypted by default too. Wraps any other configured remote — S3, B2, SMB,
  SFTP, WebDAV, Drive, etc. [[rclone.org/crypt]](https://rclone.org/crypt/)
- **Restore ergonomics**: plain file copy-back (`rclone copy`/`sync`), no built-in
  point-in-time snapshot browsing — you'd rely on the underlying remote's versioning
  (e.g., S3 object versioning) or run it alongside restic/Kopia rather than instead of
  them.
- **Footprint**: single static Go binary, MIT license, very easy to vendor.
- **Verdict**: Not a substitute for restic/Kopia/Borg, but a strong complementary
  transport layer, and a reasonable *simple* choice for mirroring the git repos
  (playbooks/IaC) where snapshot history is already provided by git itself and you
  just need an encrypted off-site copy of the bare repo.

### Litestream

- **Purpose**: Continuous WAL-streaming replication of a SQLite DB to S3-compatible
  storage (or NFS/SFTP path targets), not a general file backup tool. Directly
  addresses the "sqlite DB" part of the project's state, separately from
  config/secrets/git.
- **Encryption**: Litestream itself does **not encrypt** — it relies on the
  destination bucket's server-side encryption or a wrapping layer (e.g., point it at
  an S3-compatible endpoint reached via an encrypting proxy, or encrypt the DB at rest
  with SQLCipher before Litestream ever sees it). Confirm current docs before relying
  on any specific encryption claim; nothing found in the 2026 sources indicates
  built-in client-side encryption was added.
- **Restore ergonomics**: `litestream restore` reconstructs the DB file at a
  point-in-time from the WAL stream — this is its strongest feature relative to
  restic/Kopia/Borg, which snapshot the file periodically rather than continuously.
- **Health**: Actively maintained — v0.5.16 shipped 2026-08-05 [[litestream releases]](https://github.com/benbjohnson/litestream/releases). A VFS-based read-replica mode (fetch pages directly from S3) is in
  development [[litestream.io/how-it-works]](https://litestream.io/how-it-works/).
- **License**: Apache-2.0. Single static Go binary — fits the footprint goal well as a
  sidecar process/container.

### LiteFS

- **Purpose**: FUSE-based distributed filesystem giving multi-node SQLite replication
  (Raft-like primary/replica), built by Fly.io for their platform. S3-compatible
  backup of the FUSE-managed database is a built-in feature, separate from live
  replication.
- **Health**: Functionally frozen — **LiteFS Cloud was sunset in October 2024** and
  Fly.io has "deprioritized active development"; it's stable and usable but explicitly
  pre-1.0 beta with no forward momentum [[litestream.io/alternatives]](https://litestream.io/alternatives/).
- **Verdict for this project**: Overkill and going stale — homelab scale (one
  operator, "a handful of hosts") doesn't need multi-node SQLite HA. Litestream alone
  covers the "durable off-site copy of the SQLite DB" need with far less machinery and
  active upstream investment.

### git-crypt

- **Encryption model**: Transparent, file-level, driven by `.gitattributes` filters —
  AES-256-CTR with a synthetic IV so identical plaintext blocks don't leak patterns.
  Keys are GPG keys (per-collaborator) or a raw symmetric key file.
- **Trade-off**: Encrypted files become opaque binary blobs in git history — no
  meaningful `git diff`/code review on secret files, and GPG key management is the
  main operational cost (needs an agent, key distribution).
- **Health**: Slow-moving but not abandoned — v0.8.0 shipped 2025-09-23; project
  self-describes as still pre-1.0 with possible format breaks [[AGWA/git-crypt]](https://github.com/AGWA/git-crypt).
- **License**: GPL-3.0 (matters if this ever becomes a proprietary product, not a
  concern for a personal homelab tool the user owns outright).

### age + sops (recommended secrets layer, not "backup" per se but adjacent)

- **age**: modern, minimal `age`/`age-keygen`, X25519 or **can use existing SSH keys**
  directly (no separate keypair to manage) — genuinely lower operational overhead than
  GPG for a one-operator homelab [[Storing secrets in git with age]](https://blog.9wd.eu/posts/git-encryption-age/).
- **sops** (Mozilla): encrypts values in-place inside YAML/JSON/ENV files (not whole
  files), supports age, PGP, and cloud KMS (AWS/GCP/Azure/**Vault/OpenBao**) as key
  backends — this is the natural fit for "ansible-vault-style secrets, but for
  Terraform/general config too," and is already the de-facto GitOps secrets tool
  (used heavily by FluxCD/ArgoCD ecosystems).
- **git-agecrypt / agebox**: thinner, git-crypt-style transparent whole-file
  encryption using age instead of GPG — simpler mental model than sops if you want
  git-crypt's UX without GPG [[AGWA vs git-agecrypt comparison]](https://github.com/vlaci/git-agecrypt).
- **Verdict**: For "integrates ansible-vault, sops/age/OpenBao-class options" this is
  already the answer — sops-with-age is the standard, well-maintained glue, and it
  composes cleanly with OpenBao/Vault as a KMS backend for team/rotation scenarios if
  the homelab ever needs that.

### GitHub as a backup storage target

Two real patterns surfaced:

1. **Encrypted git bundle push**: `git bundle create` a bare repo snapshot, encrypt
   the resulting single file (age/gpg/openssl), then either commit it into a
   *separate* private GitHub repo as a binary blob, or push it via `git-remote-gcrypt`
   which makes an **entire remote transparently encrypted** — you `git push`/`git pull`
   normally and gcrypt handles encrypt-on-push/decrypt-on-fetch, storing ciphertext
   blobs GitHub can't read [[Repository Backup — Git Memo]](https://git-memo.readthedocs.io/en/latest/repository_backup.html). GPL-3.0, shell-script based, mature but low-velocity — good candidate for
   "push my ops-repo bundle to GitHub encrypted" without inventing a protocol.
2. **restic-repo-as-git-remote**: `git-remote-restic` bridges git's remote-helper
   protocol to a restic repository, so `git push restic::...` stores git objects
   inside a restic repo (which could itself live on S3/B2/NAS) — interesting but this
   is a small, low-activity hobby project; treat as a novelty to watch, not a
   dependency to build DR on [[git-remote-restic]](https://github.com/manwithafox/git-remote-restic).
3. **Plain GitHub-as-object-store**: nothing found supports using GitHub's own storage
   as an S3-style target for restic/Kopia/Borg directly — GitHub isn't a blob-store
   backend for any of them. The only viable path onto GitHub is "put an encrypted
   artifact into a git repo," which is fine for small state (a SQLite DB dump, config,
   secrets bundle — likely low tens of MB at homelab scale) but wrong for anything
   that would blow past GitHub's soft repo-size / large-file guidance.

**Practical recommendation for the GitHub leg**: skip bespoke remote-helpers. Have the
control-plane produce one encrypted, content-addressed archive per backup run (restic
or a plain `age`-encrypted tarball of a `git bundle` + SQLite dump + sops-encrypted
config) and `git push` it into a private GitHub repo as the object store, pruning old
blobs on a retention schedule. This reuses `git` itself as the transport/versioning
mechanism rather than depending on a niche remote-helper project.

---

## Integration notes against project goals

- **Single-binary/single-container**: restic, Kopia (CLI mode), rclone, Litestream,
  age, sops, git-crypt are all static Go/Rust/C binaries — trivial to vendor into one
  container image alongside the control-plane binary. Borg/borgmatic's Python
  dependency is the one real friction point if "single container, minimal layers" is
  a hard requirement (still very doable — one more `apk add py3-borgmatic`-style layer
  — just not a single static binary).
- **NFS/SMB NAS**: Solved trivially by every tool above except pure S3-only ones,
  because a mounted NFS/SMB share is just a local path to restic/Borg/rclone/
  Litestream. No special "NAS support" is needed beyond mounting the share in the
  container/host and pointing the tool at the mount.
- **S3-compatible + Backblaze**: restic and Kopia both have first-class native
  support; Borg needs rclone or SSH as a bridge. If Borg's compression/verification
  maturity is wanted, plan on running it behind `rclone mount` from day one rather
  than treating object storage as an afterthought.
- **GitHub as a target**: best solved as "push an encrypted archive via plain git,"
  not via a specialized remote-helper (see above) — keeps the dependency surface to
  `git` + `age`/`sops`, both of which the project needs anyway for secrets.
- **Go library vs shelling out**: **restic forecloses the library option by design**
  (`internal/` packages, explicitly rejected feature request) — any Go control-plane
  using restic must shell out and parse `--json`/exit codes, full stop. **Kopia is the
  only one of the majors that ships an importable Go API**, but it's pre-1.0 and the
  docs warn about instability — treat in-process embedding as a future optimization,
  not a v1 architecture bet. For a v1, **shelling out to a pinned restic binary is the
  lowest-risk choice**: stable CLI/JSON contract, huge community, and the
  process-boundary actually helps failure isolation (a backup-tool crash doesn't take
  the control-plane down) — plausibly more valuable at homelab-DR scale than saving a
  process fork.
- **SQLite specifically**: don't rely on restic/Kopia/Borg's filesystem snapshot of a
  live SQLite file — either (a) run `sqlite3 .backup` or `VACUUM INTO` to produce a
  consistent point-in-time file before handing it to restic, or (b) run Litestream as
  a continuous sidecar streaming WAL frames to S3, which gives point-in-time restore
  without needing to coordinate snapshot timing at all. Litestream + restic (for
  everything else) is a clean division of labor: Litestream owns "the DB never loses
  more than seconds of data," restic owns "everything, including old Litestream
  generations, config, and secrets, gets an encrypted off-site copy on a schedule."
- **Secrets specifically**: sops+age for config-embedded secrets (Terraform vars,
  webhook tokens, notification credentials), ansible-vault stays ansible-vault (it's
  already the standard for playbook secrets and restic backs up the vault-encrypted
  files as opaque blobs with no extra work), OpenBao/Vault is the natural upgrade path
  *if* the homelab ever needs dynamic secrets/leasing — not needed for v1.
- **Verification**: build a scheduled `restic check --read-data-subset=...` (cheap,
  rotating integrity check rather than a full read every time, given homelab
  bandwidth/cost constraints) into the same reactive-automation engine the project
  already wants for monitoring-triggered runbooks — this turns "verify backups" into
  just another scheduled/webhook-triggered job, no new subsystem needed.

---

## Recommendation

**Core backup engine: restic**, shelled out to as a pinned binary from the
control-plane process.

- Single static binary, BSD-2 license, native S3/B2/SMB-NFS(via mount)/SFTP/rclone
  backends, mature `check`/`restore`/`mount`/`dump` UX, active project
  (v0.19.1, 2026-07-05) [[restic 0.19.1]](https://restic.net/blog/2026-07-05/restic-0.19.1-released/).
- Accept the "no Go library" limitation — shell out and parse `--json`. This is a
  deliberate, permanent project stance from upstream, not a temporary gap
  [[restic#4406]](https://github.com/restic/restic/issues/4406), so design the
  integration around a CLI wrapper from the start rather than waiting for a library
  API that may never land.
- Re-evaluate **Kopia** specifically if/when in-process embedding becomes a real
  requirement (e.g., wanting live backup-run progress streamed into a TUI without
  polling a subprocess) — its `repo`/`object`/`content` Go packages are purpose-built
  for that, just not yet stable enough to bet v1 on.

**SQLite state: Litestream** as a sidecar, continuously streaming WAL to the same
S3-compatible bucket restic already targets. Layer restic's periodic encrypted
snapshot on top for the "everything, including Litestream's own generations, on a
schedule" story and for the NAS/GitHub legs Litestream doesn't cover.

**Config/git repos**: back up as plain files under restic's normal snapshot (they're
small); for the GitHub leg specifically, push a periodic `age`-encrypted archive
(git bundle + config + sops-encrypted secrets) into a private GitHub repo via plain
`git push` — skip specialized remote-helpers (`git-remote-gcrypt`,
`git-remote-restic`) for v1; they add a dependency with limited community weight for
a leg that a five-line script already covers.

**Secrets**: `sops` + `age` for config-embedded secrets; keep `ansible-vault` as-is for
playbook secrets (restic already treats vault-encrypted files as opaque ciphertext, no
extra integration needed); leave OpenBao/Vault as a documented future upgrade, not a
v1 dependency.

**Explicitly avoid for v1**: Borg/borgmatic (Python runtime friction + no native
object-storage backend outweighs its compression/verification edge at homelab scale),
rustic (upstream says not production-ready yet — revisit at 1.0), LiteFS (frozen
project, solves a multi-node HA problem this project doesn't have), and
`git-remote-restic`/`git-remote-gcrypt` (real but low-activity projects — fine to
reach for later, not to build the DR critical path on now).

---

## Sources

- [restic/restic — LICENSE](https://github.com/restic/restic/blob/master/LICENSE)
- [restic 0.19.1 released — restic.net blog, 2026-07-05](https://restic.net/blog/2026-07-05/restic-0.19.1-released/)
- [restic 0.19.0 released — restic.net blog, 2026-06-09](https://restic.net/blog/2026-06-09/restic-0.19.0-released/)
- [restic 0.14.0 released — compression added — restic.net blog, 2022-08-25](https://restic.net/blog/2022-08-25/restic-0.14.0-released/)
- [restic docs — Preparing a new repository (backends)](https://restic.readthedocs.io/en/latest/030_preparing_a_new_repo.html)
- [Please allow restic to be used as a "Go library" — restic/restic#4406](https://github.com/restic/restic/issues/4406)
- [pkg.go.dev — github.com/restic/restic/internal](https://pkg.go.dev/github.com/restic/restic/internal)
- [Automated AES-256 backups with restic + rclone — botmonster.com](https://botmonster.com/self-hosting/set-up-automatic-encrypted-backups-restic-rclone/)
- [borgbackup.org/releases](https://www.borgbackup.org/releases/)
- [Borgmatic vs Restic: Which Backup Tool to Use? — dev.to](https://dev.to/selfhostingsh/borgmatic-vs-restic-which-backup-tool-to-use-4bgo)
- [kopia/kopia — LICENSE (Apache-2.0)](https://github.com/kopia/kopia/blob/master/LICENSE)
- [pkg.go.dev — github.com/kopia/kopia/repo](https://pkg.go.dev/github.com/kopia/kopia/repo)
- [rustic — docs.rs, latest package docs (beta status)](https://docs.rs/crate/rustic-rs/latest)
- [rustic-rs/rustic — GitHub](https://github.com/rustic-rs/rustic)
- [rustic_core — library for rustic-rs — GitHub](https://github.com/rustic-rs/rustic_core)
- [rclone — Crypt remote docs](https://rclone.org/crypt/)
- [rclone — Licence](https://rclone.org/licence/)
- [benbjohnson/litestream — Releases](https://github.com/benbjohnson/litestream/releases)
- [Litestream — How it works](https://litestream.io/how-it-works/)
- [Litestream — Alternatives (LiteFS comparison, Fly.io status)](https://litestream.io/alternatives/)
- [Fly.io — Introducing LiteFS](https://fly.io/blog/introducing-litefs/)
- [AGWA/git-crypt — GitHub (license, v0.8.0 date)](https://github.com/AGWA/git-crypt)
- [Storing secrets in git repositories with age — blog.9wd.eu](https://blog.9wd.eu/posts/git-encryption-age/)
- [git-agecrypt — vlaci/git-agecrypt](https://github.com/vlaci/git-agecrypt)
- [slok/agebox — age-based repository file encryption](https://github.com/slok/agebox)
- [Repository Backup — Git Memo docs (git bundle / gcrypt patterns)](https://git-memo.readthedocs.io/en/latest/repository_backup.html)
- [manwithafox/git-remote-restic — GitHub](https://github.com/manwithafox/git-remote-restic)
