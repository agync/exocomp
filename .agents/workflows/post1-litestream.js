export const meta = {
  name: 'exocomp-post1-litestream',
  description: 'Post-v1 #1: Litestream continuous WAL replication (replicate + restore lanes)',
  phases: [{ title: 'Build', detail: 'replicate (opus) + restore (opus)' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const WT = REPO + '/.worktrees'
const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/post-v1-plan.md (item 1, Litestream) FIRST. Also BINDING: docs/adr/0011-project-dependency-resolution.md (house ADR style), docs/operations/disaster-recovery.md (the existing restic DR story you complement, do NOT replace), docs/sbom/bill-of-materials.md (Litestream is already listed as an optional sidecar), docs/adr/0001-thin-core-architecture.md (anti-scope: no second heavy service inside the daemon — Litestream runs BESIDE exocomp as a sidecar, exocomp generates its config and observes it, it is NOT compiled in and NOT run inside exocompd).
GOVERNING FACTS: Litestream gives seconds-level RPO by shipping SQLite WAL frames to a replica target; it is the FAST-RPO COMPLEMENT, restic stays the AUTHORITATIVE encrypted DR chain (keep both; do not conflate). Litestream does NOT encrypt — a replica target must be LAN-local or an already-encrypted store; refuse/loudly-warn a plaintext remote target without an explicit ack. litestream is NOT installed on this host: build so the config-gen + restore logic are DETERMINISTICALLY UNIT-TESTED (no litestream binary needed), and any e2e that shells litestream SKIPs cleanly when the binary is absent (like the tofu/ansible gates skip).
Conventions: lane-owned paths ONLY; logical-unit commits via "git -C <worktree>"; conventional-commit titles (feat|fix|chore|docs|refactor|test|perf|build)(scope): summary, <= 52 chars; NO trailers; explicit staging (never git add -A); no-slop comments (WHY not WHAT); never sudo. The pre-commit hook runs the FULL make check (go vet + go test -race ./..., ~2.5 min) on EVERY commit — never bypass, be patient, expect commit-msg to reject a >52-char or non-conventional title. REUSE: the store setting/secret CRUD, the doctor optionalTools + staleness-gauge patterns (internal/metrics), the backup engine (internal/backup: RestoreLatest, LocalRestore) and the recovery kit. Secrets never in argv/env-dumps/persisted-plaintext; a replica target's credentials go through the sealed secret plane, not a plaintext config on disk.
Host: go 1.26, golangci-lint, tofu, ansible, restic, age, sops, uv, podman, nono. Never sudo.
`
const SCHEMA = {
  type: 'object', required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' }, verified: { type: 'array', items: { type: 'string' } }, concerns: { type: 'array', items: { type: 'string' } },
  },
}
phase('Build')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: replicate. Worktree: ${WT}/post1-ls-replicate (branch post1-ls-replicate; all git via "git -C ${WT}/post1-ls-replicate"). Own EXCLUSIVELY: a new internal/litestream/** package; cmd/exocomp/litestream.go (self-register via init/registerCommand — do NOT edit main.go); cmd/exocomp/doctor.go (add a litestream probe + RPO gauge line); internal/metrics/** (a replication-freshness / RPO-age staleness gauge); docs/adr/0012-litestream.md + docs/adr/index.md row; a Litestream refinement to docs/sbom/bill-of-materials.md if the row needs it. Do NOT touch internal/backup, cmd/exocomp/restore.go, or docs/operations/disaster-recovery.md (the restore lane's).

Build:
1. ADR 0012 (house style Status/Context/Decision/Consequences): Litestream as an OPTIONAL fast-RPO sidecar replicating exocomp.db's WAL; restic stays the authoritative encrypted DR chain; the no-encryption caveat (LAN-local or encrypted target only); sidecar-not-embedded (anti-scope). Update docs/adr/index.md.
2. internal/litestream: generate a litestream.yml config from exocomp's DB path + a configured replica target (a local/NFS dir, or an S3/SFTP endpoint). A target's credentials resolve from the sealed secret plane, never plaintext in the emitted config where avoidable (document the seam). GenerateConfig is DETERMINISTIC and unit-tested (golden config for a local target and an s3 target). A plaintext-remote target without an explicit ack is refused/loudly-warned (encryption caveat) — unit-tested both ways.
3. cmd/exocomp/litestream.go: \`litestream config\` (emit the generated config to stdout/a path), \`litestream target <url> [--ack-unencrypted]\` (persist the replica target), \`litestream status\` (report the binary presence + replication freshness/RPO age read from the replica, or "sidecar not running"). Refuse cleanly when the binary is absent, naming the install step.
4. doctor + metrics: add a litestream probe to doctor (present? replicating? RPO age) and a replication-freshness staleness gauge to /metrics (age of the last replicated WAL frame; honest "unknown" when no replica configured).

GATE (paste REAL output): golden config generation for a local + an s3 target; the unencrypted-remote refusal (and the --ack override); \`litestream status\`/doctor honest output with the binary absent (SKIP the shell-litestream e2e). Full make check green. Report with the SCHEMA. Do NOT touch the restore lane's files.`,
    { label: 'lane:replicate', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: restore. Worktree: ${WT}/post1-ls-restore (branch post1-ls-restore; all git via "git -C ${WT}/post1-ls-restore"). Own EXCLUSIVELY: the fast-RPO restore path in internal/backup/** (a NEW file preferred, e.g. internal/backup/litestream_restore.go — do NOT rewrite restore.go/engine.go, call/extend them minimally); cmd/exocomp/restore.go (a --from-litestream path or a sibling command); docs/operations/disaster-recovery.md (a fast-RPO section); the recovery-kit note (the replica location on the card). Do NOT touch internal/litestream, cmd/exocomp/{litestream,doctor}.go, internal/metrics, or docs/adr (the replicate lane's).

Build:
1. The restore path: restore exocomp.db from the Litestream replica (shell \`litestream restore\`), reconciled with restic RestoreLatest (internal/backup): document + enforce PRECEDENCE — Litestream for the freshest (seconds-RPO) recovery of exocomp.db, restic for the authoritative, ENCRYPTED, full DR (keyring, secrets, everything). A Litestream restore recovers the DB only; the recovery flow must still verify the envelope opens (decrypt round-trip) before trusting it. Unit-test the decision logic (which source, precedence, the DB-only-vs-full distinction) deterministically WITHOUT the litestream binary; the actual shell round-trip is an e2e that SKIPs when litestream is absent.
2. cmd/exocomp/restore.go: \`--from-litestream\` (or \`restore litestream\`) restores the DB from the configured replica, refuses cleanly when the binary or a replica is absent, and REFUSES to auto-resume schedules (paused restore, matching the existing recovery posture).
3. docs/operations/disaster-recovery.md: a "Fast-RPO restore (Litestream)" section — when to use it vs restic, the RPO/RTO difference, the no-encryption caveat, and the DB-only scope. Add the replica location to the recovery card so a rebuild can find it.

GATE (paste REAL output): the source-selection/precedence unit tests (Litestream=DB-only fast, restic=authoritative encrypted, decrypt-round-trip gate before trust); \`restore --from-litestream\` honest refusal with the binary/replica absent; the e2e round-trip SKIPs cleanly when litestream is absent (state it). Full make check green. Report with the SCHEMA. Do NOT touch the replicate lane's files.`,
    { label: 'lane:restore', phase: 'Build', model: 'opus', schema: SCHEMA }),
])
return { lanes: results.filter(Boolean) }
