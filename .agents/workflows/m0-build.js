export const meta = {
  name: 'exocomp-m0-foundations',
  description: 'M0 Foundations: four parallel worktree lanes scaffold the exocomp repo',
  phases: [
    { title: 'Build', detail: 'docs-okf (opus), go-skeleton, agents-infra, packaging-lima (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const CONVENTIONS = `
REPOSITORY CONVENTIONS (violations fail integration review — read ${REPO}/AGENTS.md first):
- Work ONLY inside your assigned worktree directory. Never touch ${REPO} itself, other worktrees, or any path outside your worktree (read-only reading of source material paths given below is fine).
- Commit as you go in LOGICAL UNITS using "git -C <your-worktree>": conventional-commits style, title <= 52 chars, terse body stating what the change does. Stage explicit paths only — never "git add -A" or "git add .".
- ABSOLUTELY NO commit trailers: no Co-Authored-By, no Signed-off-by, no Claude-Session, no "Generated with" lines, no emoji.
- No AI slop: code comments only for constraints the code cannot show; no narrating comments; no filler or marketing prose in any file.
- Never use sudo. Never install anything system-wide. User-scope tooling (uv tool install, go install to GOPATH) is allowed.
- Verification before completion: run your verification commands, and report their real output in your structured result. Do not claim success you did not observe.
Host tools available: go 1.26.5, uv 0.9.7, limactl 2.2.0, podman 5.8.4, git 2.55, GNU make 4.4.1. golangci-lint and ansible are NOT installed on the host.
`

const SCHEMA = {
  type: 'object',
  required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' },
    verified: { type: 'array', items: { type: 'string' }, description: 'verification commands actually run, each with a one-line real result' },
    concerns: { type: 'array', items: { type: 'string' }, description: 'anything the integrator must know: deviations, deferred bits, sharp edges' },
  },
}

phase('Build')
log('4 lanes in worktrees: docs (opus) · go · agents-infra · packaging-lima (sonnet)')

const results = await parallel([
  () => agent(`${CONVENTIONS}
YOUR LANE: docs-okf. Worktree: ${WT}/m0-docs (branch m0/docs).
Build the repository's docs/ directory as an OKF v0.2 bundle, curated from the pre-repo design archive, path supplied at run time (READ-ONLY source; authoritative file: design/DESIGN.md; supporting: research/*.md, design/candidate-*.md, design/critique.md).

Deliverables inside <worktree>/docs/:
1. index.md — root index with frontmatter "okf_version: \\"0.2\\"" plus title/description metadata; a high-signal map of every section with relative links.
2. log.md — Knowledge Base Evolution log (OKF scope: page additions/deprecations/structure — NEVER software release notes). First entry: bundle created by curation from the pre-repo design archive, dated 2026-08-21.
3. adr/ — index.md + one ADR per decision, format: Status/Context/Decision/Consequences, concise and specific. Write these six from the archive: 0001 thin-core architecture (vs compose-first/hybrid, why), 0002 project name exocomp (Control D ctrld collision, exocomp.dev), 0003 secrets envelope (age recipients -> DEK -> HKDF; recovery kit incl. read-only storage cred), 0004 OpenTofu HTTP state backend in SQLite (MinIO lockfile gap, version history), 0005 job sandboxing via nono local-sandboxed tier (CVE lesson: deny local sockets), 0006 uv build-time toolchain + operator escape hatch (UV_TOOL_DIR in data volume, doctor-gated).
4. architecture/ — index.md + overview.md (components, one-queue convergence, the mermaid diagram from DESIGN.md adapted), platform-contract.md (systemd/podman/docker-compose/SELinux tiers from DESIGN §7.4), execution.md (ExecutorDriver tiers local/local-sandboxed/container from §7.2-7.3).
5. design/ — index.md + requirements.md (the six goals + hard constraints), amendments.md (the 12 critique-driven amendments, condensed), delivery.md (milestone table from DESIGN §5 with effort).
6. operations/ — index.md + disaster-recovery.md (the DR runbook + recovery chain; the recovery-card commands), bootstrap.md (install + six-screen wizard flow).
7. sbom/ — index.md + bill-of-materials.md (all five BOM layers from DESIGN §4 incl. budgets and the GPL subprocess-boundary rule).
8. history/ — index.md explaining these are verbatim provenance records using the pre-rename working name "control"; copy in the 8 research notes, the addendum, the 3 candidate designs, and critique.md unchanged except a one-line provenance header comment is NOT needed — import them byte-identical and explain naming in history/index.md.

OKF hygiene (hard requirements): every cross-reference is a RELATIVE markdown link; ZERO absolute paths, /home/<user> strings, hostnames, or file:// URLs anywhere in curated pages — if source text contains them, rewrite with relative references or generic placeholders (history/ verbatim imports are exempt ONLY if they already contain none — check with grep; if any absolute home paths exist in a history file, replace just those strings with a neutral placeholder and note it in history/index.md). No marketing language. High signal density; shorter than the source, never vaguer.
Commit in logical units (e.g. adr set, architecture, operations, sbom, history import). Verify: grep -r for '/home/' and 'file://' across docs/ returns nothing (report the command output); every relative link resolves (spot-check with a shell loop and report).`,
    { label: 'lane:docs-okf', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${CONVENTIONS}
YOUR LANE: go-skeleton. Worktree: ${WT}/m0-go (branch m0/go).
Create the Go foundation. Module path: exocomp.dev/exocomp. Go 1.26.

Deliverables:
1. go.mod; cmd/exocomp/main.go — cobra CLI with subcommands: "version" (prints version/commit/date injected via -ldflags, defaults "dev"), "doctor" (real minimal checks that run today: go runtime version, data dir writability under $XDG_DATA_HOME or ~/.local/share/exocomp, presence+version probe of optional tools ansible-playbook/tofu/restic/age/sops with clear MISSING output — missing tools are warnings not errors in M0), "serve" (returns a clear not-implemented-in-M0 error, exit 1).
2. internal/version/ (ldflags target), internal/service/, internal/queue/, internal/secrets/, internal/executor/, internal/api/, internal/tui/, internal/web/ — each with an idiomatic one-line package doc.go stating the package's responsibility (from AGENTS.md layout); no placeholder logic, no TODO comments.
3. Makefile following the pattern: .DEFAULT_GOAL := help; a "help" target that greps ## comments; targets: build (static, CGO_ENABLED=0, ldflags version injection, output bin/exocomp), check (fmt-check + vet + lint + test), fmt, vet, lint (uses golangci-lint if on PATH else prints "golangci-lint not installed — skipping" and exits 0), test (go test ./...), clean. Tabs, .PHONY correct.
4. .editorconfig (go: tabs; yaml/md: 2 spaces); .golangci.yml minimal sane config (enable govet, staticcheck, errcheck, revive) for when the tool exists.
5. A trivial go test somewhere real (e.g. internal/version) so "make test" exercises the toolchain.
Verify and report real output: make build && ./bin/exocomp version && ./bin/exocomp doctor; make check; go vet ./... clean.`,
    { label: 'lane:go-skeleton', phase: 'Build', model: 'sonnet', schema: SCHEMA }),

  () => agent(`${CONVENTIONS}
YOUR LANE: agents-infra. Worktree: ${WT}/m0-agents-infra (branch m0/agents-infra).
Create the .agents/ automation directory and the local-shim bootstrap.

Deliverables:
1. .agents/hooks/commit-msg — POSIX sh, executable. Rejects with a clear message when: first line exceeds 52 chars; first line does not match conventional style ^(feat|fix|chore|docs|refactor|test|perf|build)(\\([a-z0-9./-]+\\))?: .+ ; any line matches ^(Co-Authored-By|Co-authored-by|Signed-off-by|Claude-Session|Generated-with|Generated with):? ; message contains an emoji from the common ranges (use a conservative grep for the robot/sparkle emojis rather than full unicode classes). Must pass messages that are legitimate (test it).
2. .agents/hooks/pre-commit — POSIX sh, executable: if a Makefile with a check target exists at the worktree root, run "make check" and fail the commit on failure; otherwise exit 0 with a note. Keep it fast and quiet on success.
3. .agents/bootstrap.sh — POSIX sh, executable, idempotent: sets "git config core.hooksPath .agents/hooks"; creates CLAUDE.md (content exactly: "Read AGENTS.md — it is the entrypoint and contract for this repository.") if absent; creates .claude/ and inside it relative symlinks agents -> ../.agents/agents and skills -> ../.agents/skills (replace only if already correct symlinks; never clobber real dirs). Prints what it did.
4. .agents/agents/ — three short role cards (markdown, <25 lines each, no fluff): lane-implementer.md (worktree discipline, conventions, verification-before-completion), adversarial-reviewer.md (tries to refute claims; checks invariants list in AGENTS.md; spec pass then quality pass), docs-curator.md (OKF v0.2 rules: relative links, no host paths, log.md scope).
5. .agents/skills/repo-conventions/SKILL.md — frontmatter name+description, body: the commit rules (52-char title, conventional prefixes, no trailers, logical staging), the no-slop rules, worktree workflow, hooksPath activation. This is the machine-discoverable copy of AGENTS.md invariants 9-12.
6. .agents/workflows/README.md — two paragraphs: milestone workflows and maintenance workflows live here as .js Workflow scripts; naming convention mNN-build.js / mNN-maintain.js.
Verify and report: run bootstrap.sh in the worktree and show the symlinks; feed the commit-msg hook a bad title, a trailer, and a good message via temp files and show pass/fail results; shellcheck if available (it may not be — say so).`,
    { label: 'lane:agents-infra', phase: 'Build', model: 'sonnet', schema: SCHEMA }),

  () => agent(`${CONVENTIONS}
YOUR LANE: packaging-lima. Worktree: ${WT}/m0-packaging (branch m0/packaging).
Create packaging skeletons and a WORKING lima test-node harness.

Deliverables:
1. packaging/Containerfile — honest multi-stage skeleton: build stage (golang:1.26, CGO_ENABLED=0 build of ./cmd/exocomp — the cmd tree lands from another lane, so guard with a comment-free build that will work post-merge), runtime stage (registry.fedoraproject.org/fedora-minimal or debian-slim; COPY binary; placeholder RUN lines for the uv toolchain layer kept minimal). It must be syntactically valid for "podman build" even if the build would fail pre-merge; do NOT run podman build against it in this lane.
2. packaging/quadlet/exocomp.container + exocomp.volume — working Quadlet units per current podman 5.x syntax: rootless, MemoryMax=1.5G, volume mount, Type=notify placeholder commented OUT (binary lacks sd_notify in M0) with a plain always-restart config active.
3. packaging/compose.yaml — equivalent fallback.
4. packaging/install.sh — POSIX sh skeleton, separately versioned (VERSION=0.0.1 var): detects podman vs docker, prints the chosen tier and what it WOULD do for install/restore/rollback modes (real logic lands in later milestones); --help works; never sudo.
5. test/lima/node-fedora.yaml — lima template based on template://fedora: 2 CPU, 2GiB, no host mounts writable, name-agnostic.
6. test/lima/smoke.sh — executable: (a) limactl start --name exocomp-node0 -y ./node-fedora.yaml if not running (this DOWNLOADS a Fedora image on first run — allow up to 15 minutes, do not abort early); (b) waits for ssh readiness; (c) limactl shell exocomp-node0 -- uname -a; (d) installs ansible-core USER-SCOPE on the host via "uv tool install ansible-core" if ansible-playbook absent; (e) writes an ssh config via limactl show-ssh and a minimal inventory, then runs "ansible -m ping" against the node and asserts SUCCESS; (f) prints PASS/FAIL summary; (g) accepts "stop" subcommand that limactl stops the node. Never sudo anywhere.
7. test/lima/README.md — 10 lines max: what the node is for, how to run smoke, how to stop/delete.
EXECUTE the smoke test for real (first boot may take minutes) and report the actual uname line and ansible ping result in "verified". Leave the VM created but STOPPED at the end (run the stop path).`,
    { label: 'lane:packaging-lima', phase: 'Build', model: 'sonnet', schema: SCHEMA }),
])

const done = results.filter(Boolean)
log(`Lanes complete: ${done.length}/4`)
return { lanes: done }
