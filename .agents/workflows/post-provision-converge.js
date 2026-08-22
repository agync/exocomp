export const meta = {
  name: 'exocomp-provision-converge',
  description: 'Provision->onboard->converge chain + standing story-5 inventory (v1.1 A+B)',
  phases: [{ title: 'Build', detail: 'provision-converge (opus)' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const WT = REPO + '/.worktrees'
const SCHEMA = {
  type: 'object', required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' }, verified: { type: 'array', items: { type: 'string' } }, concerns: { type: 'array', items: { type: 'string' } },
  },
}
phase('Build')
const results = await parallel([
  () => agent(`Read <worktree>/AGENTS.md, docs/design/v1.1-roadmap.md ("TF+Ansible operational coverage" — items A and B you implement), docs/design/v1-acceptance.md (story 5), docs/adr/0010-source-and-credential-model.md, docs/operations/terraform-ansible-handoff.md FIRST.
YOUR LANE: provision-converge. Worktree: ${WT}/post-provision-converge (branch post-provision-converge; all git via "git -C ${WT}/post-provision-converge"). This extends the SHIPPED v1.0 story-5 handoff + host onboarding into the highest homelab-leverage v1.1 feature. Own EXCLUSIVELY: cmd/exocomp/{handoff.go, host.go} and a NEW cmd/exocomp/provision.go if cleaner; internal/executor/tofu_output.go (extend the capture); internal/store/tofuinventory.go (extend the standing-inventory persistence); internal/sched/** (let a schedule target a workspace-derived inventory). Do NOT touch internal/backup, internal/litestream, cmd/exocomp/{litestream,restore,doctor}.go, internal/metrics, or docs/adr (other lanes' / running work).

BINDING FACTS you build on (v1.0, already merged): the step-ledger + queue.Sequencer; story-5 materializes declared non-sensitive tofu outputs into a \`tf:<workspace>\` inventory row (internal/store/tofuinventory.go MaterializeInventory) + an inventory artifact consumed from a 0700 dir with no bearer/state; host onboarding (cmd/host.go) runs a seeded runbook that pushes the fleet key, creates the automation user, records+backs-up known_hosts; \`exocomp handoff run\` chains apply->play as a ledger workflow. Conventions: lane-owned paths ONLY; logical-unit commits; conventional titles <=52 chars; NO trailers; explicit staging; never sudo; the pre-commit hook runs the FULL make check (~2.5 min) on every commit — be patient.

BUILD (items A + B):
A. **Provision -> auto-onboard -> converge, one resumable chain.** A command (extend \`handoff run\` or a new \`provision run <workspace> <playbook>\`) that is a step-ledger workflow: (1) tofu apply + capture outputs (story-5); (2) DERIVE the new host(s) from the declared outputs and AUTO-RUN the onboarding runbook against them — push the fleet key, create the user, record known_hosts, WAITING for SSH to actually come up (a reachability/wait step, bounded); (3) materialize the inventory; (4) run the configure play. Step N+1 only if N succeeded; a crash resumes from the ledger (reuse the sequencer). The onboarding + configure steps are ansible-kind jobs; do not invent a new driver.
B. **Standing inventory for scheduled/reactive plays.** Make the \`tf:<workspace>\` inventory a first-class, refreshed-on-apply standing inventory a SCHEDULE (internal/sched) or a reactive rule can target by name, so a freshly-provisioned host auto-joins the recurring converge (the day-2 freshness the handoff alone does not give). Refresh the inventory whenever the workspace applies (already true for daemon applies — extend to the chain), and let \`schedule add ... --kind <play-on-inventory>\` (or the existing shell schedule invoking \`playbook run --inventory tf:<ws>\`) consume it. Keep it honest: a stale/empty inventory fails closed, never runs a play against nothing.

GATE (paste REAL transcripts): the provision->onboard->converge chain proven end-to-end against a reachable target the gate can use — either the lima node (test/lima) as the "provisioned" host (a workspace whose outputs name the lima node's ssh endpoint, then onboard+converge it) OR localhost — showing the four ledger steps, the auto-onboard waiting for SSH, and the configure play running against the tf-derived inventory; a crash mid-chain resumes from the ledger. Then a SCHEDULED play consuming \`tf:<ws>\` runs after a refresh (a new host in the inventory is converged on the next tick). A new test/e2e/provision-converge-gate.sh. Full make check green. Report with the SCHEMA. A NEW docs/operations/provision-converge.md.`,
    { label: 'lane:provision-converge', phase: 'Build', model: 'opus', schema: SCHEMA }),
])
return { lanes: results.filter(Boolean) }
