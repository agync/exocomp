export const meta = {
  name: 'exocomp-m3c-maintain',
  description: 'M3c maintenance: reliability fixes, the four v1 folds, lifecycle wiring + docs',
  phases: [{ title: 'Maintain', detail: 'rel (opus) + exec (opus) + life (opus)' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const WT = REPO + '/.worktrees'
const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md (the "M3c maintenance backlog" section = the numbered items you own) FIRST. This is a MAINTENANCE pass on merged M3c: harden reliability and land the four v1 "cheap folds". Conventions: lane-owned paths ONLY; logical-unit commits via "git -C <worktree>"; conventional-commit titles (feat|fix|chore|docs|refactor|test|perf|build)(scope): summary, <= 52 chars; NO trailers; explicit staging (never git add -A); no-slop comments (WHY not WHAT); never sudo. The pre-commit hook runs the FULL make check (go vet + go test -race ./..., ~2-2.5 min) on EVERY commit — never bypass, be patient, expect commit-msg to reject a >52-char or non-conventional title. REUSE existing machinery: the queue is the ONLY executor; the step ledger + Sequencer (internal/{store,queue}/workflow.go); the secrets Envelope/Derive/Redactor + the run-secret named-env plane; the approval gate (internal/queue/gate.go); the sandbox tier; the known_hosts store (internal/store/knownhost.go incl. ErrKnownHostChanged + RotateKnownHost from M3b). Subprocess success is exit-code-only; secrets never in argv/env-dumps/events/persisted-plaintext. Verify with REAL command output; add a mutation-check to any test that could pass vacuously.
Host: go 1.26, golangci-lint, tofu, ansible-playbook + ansible-galaxy, restic, age, sops, uv, podman, limactl, nono 0.74.0. Never sudo. Tofu provider cache warmed at \${TMPDIR}/exocomp-tofu-plugin-cache.
`
const SCHEMA = {
  type: 'object', required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' }, verified: { type: 'array', items: { type: 'string' } }, concerns: { type: 'array', items: { type: 'string' } },
  },
}
phase('Maintain')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: rel (queue + upgrade reliability). Worktree: ${WT}/m3c-maint-rel (branch m3c-maint-rel; all git via "git -C ${WT}/m3c-maint-rel"). Own EXCLUSIVELY: internal/queue/workflow.go, cmd/exocomp/{upgrade,rollback}.go (+ their tests). Do NOT touch internal/executor, internal/backup source, cmd/{playbook,handoff,ansible,run}.go, internal/deps, internal/hooks, or docs.

Fix backlog items 1, 2, 4:
- **Item 2 (INTEGRATION BUG, priority).** story5 wired Sequencer.ResumeAll into serve.go's boot; upgrade drives its own steps inline with an \`upgradeDriver\` kind the DAEMON queue does not register. A hard crash mid-upgrade leaves a pending upgrade workflow the daemon's boot ResumeAll would try to re-drive and FAIL on an unknown kind. Fix: make ResumeAll SKIP workflows whose step kinds the daemon queue cannot execute (or mark operator-inline workflows — upgrade — non-daemon-resumable via a workflow-kind/flag the ledger already can carry), so the daemon never trips over an interrupted upgrade. Keep the queue the sole executor. Prove with a test: an interrupted upgrade-kind workflow is left untouched by ResumeAll (not failed, not re-run), while a normal chain still resumes.
- **Item 1 (verify).** Confirm the crash-resume idempotency contract (commit 316a368) holds for the upgrade lane's steps: a resumed/retargeted step must not double-apply. Add/adjust a test if a gap exists; otherwise state it holds with evidence.
- **Item 4.** \`exocomp rollback\` restores the pre-upgrade DATA snapshot, discarding DB changes since the upgrade. Make this LOUDER: a clear warning naming what is discarded (jobs/backups/workspaces/secrets added since), and take a SAFETY snapshot of the current DB before the rollback restore (so a mistaken rollback is itself reversible), using the existing backup LocalSnapshot (call it, do not edit internal/backup).

GATE: paste real test output; full make check green. Report with SCHEMA. Do NOT edit files outside your lane.`,
    { label: 'lane:rel', phase: 'Maintain', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: exec (executor folds + the flaky test). Worktree: ${WT}/m3c-maint-exec (branch m3c-maint-exec; all git via "git -C ${WT}/m3c-maint-exec"). Own EXCLUSIVELY: internal/executor/** (ansible.go, tofu.go, tests incl. ansible_fidelity_test.go), internal/store/knownhost.go (+ test), and the secrets-sealing call site for tofu outputs (reuse the Envelope; add a NEW file if you need store/secret glue, do not fight another lane). Do NOT touch internal/queue, cmd/**, internal/backup, internal/hooks, internal/deps, serve.go, or docs.

Land folds 8a, 8c, 8d and fix the flaky test (item 3):
- **8a — sensitive tofu output → sealed secret sink.** Today the story-5 capture DROPS \`sensitive = true\` outputs (internal/executor/tofu_output.go). Instead, SEAL each sensitive output into the envelope as a run-secret (a stable name like \`tf-output/<workspace>/<name>\`), so a downstream step can BIND it as named env via the existing run-secret plane — the secrets twin of story 5. Non-sensitive outputs still become the inventory; sensitive ones NEVER reach an event or the inventory (assert), only the sealed store. Prove: a sensitive output lands sealed (openable via the envelope) and is absent from inventory + events; redaction holds.
- **8c — ansible \`--check\` structured drift.** Extend the drift concept (today tofu-only) to Ansible: a check-mode run (\`ansible-playbook --check --diff\`) parses changed-task counts per host into a structured, resource-level drift outcome (like TofuPlanOutcome), emitted as a job_event — 'N hosts clean, host X: M tasks would change'. Do not change how a normal (non-check) run behaves.
- **8d — host-key re-pin runtime detection/quarantine.** Extend the M3b ErrKnownHostChanged primitive: when an ansible run's connection hits a known_hosts MISMATCH against the recorded pin, classify it as a security signal — quarantine that host (block its concurrency key / mark it) and require an approval-gated re-pin (RotateKnownHost) rather than silently failing or auto-accepting. Prove old-vs-new fingerprint surfaces and the host is quarantined until an approved re-pin.
- **Item 3 — flaky TestAnsibleRunsFromCheckoutDir** (internal/executor/ansible_fidelity_test.go): a 'text file busy' fork/exec race writing+exec'ing a fake tool under -race. Harden (write-close-fsync before exec, or a distinct pre-created path) so it is deterministic; prove by running it -count=20 -race green.

GATE: paste real output for each; full make check green (run the flaky test x20 to prove). Report with SCHEMA. Do NOT edit files outside your lane.`,
    { label: 'lane:exec', phase: 'Maintain', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: life (lifecycle wiring + backup quiesce + deps polish + docs). Worktree: ${WT}/m3c-maint-life (branch m3c-maint-life; all git via "git -C ${WT}/m3c-maint-life"). Own EXCLUSIVELY: cmd/exocomp/{playbook,handoff}.go (hooks wiring), internal/backup/** (quiesce hooks — a NEW file preferred), internal/hooks/** (if the backup quiesce reuses it), internal/deps/parse.go (pyproject), packaging/ (MANIFEST.json), and docs/**. Do NOT touch internal/executor, internal/queue, cmd/{upgrade,rollback,ansible,run,doctor,vendor}.go, or store/knownhost.go.

Land items 9, 8b, 10, 11, 5, 6, 13:
- **Item 9 — wire hooks + gate into \`playbook run\` and \`handoff run\`.** Pass 3 shipped \`exocomp run\` composing pre-hooks → play → post-hooks → gate. Make \`playbook run\` and \`handoff run\` ALSO honor a source's \`.exocomp/hooks.yml\` (compose the same ledger workflow via the Sequencer). Factor the composition so all three commands share it (reuse cmd/run.go's helper; do not duplicate). A source with no hooks.yml behaves exactly as today.
- **8b — backup quiesce pre/post hooks.** Let a backup step carry pre/post quiesce hooks (pg_dump / stop-stack / sqlite .backup then un-quiesce) via the pass-3 hook mechanism, so a live-DB backup is application-consistent; a failed quiesce hook must ABORT the backup (never a torn snapshot) and alert. New file in internal/backup; reuse internal/hooks.
- **Item 10 — pyproject.toml parsing.** internal/deps/parse.go is a bounded PEP-621 array reader. Decide: either add a real TOML parser (a small, well-known lib — justify the go.mod add) OR document the supported subset precisely and make unsupported forms fail LOUD (not silently fewer deps). Prefer correctness; a TOML lib is acceptable if minimal.
- **Item 11 — MANIFEST.json.** The image ansible-core pin defaults to a compiled constant; commit/generate a MANIFEST.json (the frozen toolchain pins per ADR 0006) so the pin source is explicit, and have the build write it. Keep the compiled default as fallback.
- **Docs (items 5, 6, 13):** docs/log.md M3c entries (ledger, plan-policy, handoff, upgrade/rollback, deps/venv, hooks/gate) + docs/operations/index.md links for the new operator pages (plan-policy, terraform-ansible-handoff, upgrade, run-lifecycle); a source-path-not-under-data-dir note in the source/operations docs (item 5); the networked-hook boundary note (item 13, likely already in run-lifecycle.md — verify).

GATE: paste real output (playbook run honoring a hook; a quiesce-abort; pyproject parse; doctor/manifest); full make check green. Report with SCHEMA. Do NOT edit files outside your lane.`,
    { label: 'lane:life', phase: 'Maintain', model: 'opus', schema: SCHEMA }),
])
return { lanes: results.filter(Boolean) }
