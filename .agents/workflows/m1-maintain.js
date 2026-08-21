export const meta = {
  name: 'exocomp-m1-maintain',
  description: 'M1 maintenance: adversarial review + fresh-clone gate verification',
  phases: [
    { title: 'Review', detail: 'adversarial reviewer (opus) + fresh-clone verifier (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args

const FINDINGS = {
  type: 'object',
  required: ['findings', 'summary'],
  properties: {
    summary: { type: 'string' },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['severity', 'file', 'claim', 'evidence'],
        properties: {
          severity: { type: 'string', enum: ['blocker', 'should-fix', 'nit'] },
          file: { type: 'string' },
          claim: { type: 'string' },
          evidence: { type: 'string' },
          suggested_fix: { type: 'string' },
        },
      },
    },
  },
}

phase('Review')
const [review, verify] = await parallel([
  () => agent(`Adversarial reviewer for exocomp at ${REPO}, milestone M1 ("provable core": secrets envelope, store/queue, ansible executor with redaction, three-leg backup/restore, TUI, e2e gate at test/e2e/m1-gate.sh). Read AGENTS.md (12 invariants = rubric) and .agents/workflows/m1-plan.md. Read-only: modify or commit NOTHING. Your job: refute "M1 meets its bar".

Attack with priority on cross-lane seams that no single lane could see:
1. SECURITY: trace secret material end to end — grep for any path where a plaintext secret, DEK, or derived password could reach job_event, artifacts, logs, argv (visible in /proc), tempfiles outside 0700 dirs, or committed files. Check internal/backup/crypto.go (the duplicate HKDF/GCM implementation) against internal/secrets for drift risk; check the redactor is wired on EVERY event path that can carry secrets (playbook.go yes — what about devrun.go and future callers? is the raw store sink reachable?).
2. CORRECTNESS: the queue's outcome mapping vs the state machine; restore's manifest verification (can a tampered snapshot.db slip through?); backup independence claims; VacuumInto during concurrent writes; the restore refusal guards.
3. CONTRACTS: docs/ vs code after M1 (docs/log.md corrections landed — is anything else now false? operations/bootstrap.md? sbom BOM rows vs actual go.mod deps — charm.land paths!); AGENTS.md layout map vs the tree; .agents/workflows/m1-plan.md gate items vs what tests actually assert.
4. QUALITY: AI-slop sweep over new code comments; test honesty (any test that asserts nothing or can never fail? spot-check 5 by mutating mentally); cmd/exocomp consistency (flag naming, error wording, exit codes across playbook/backup/restore/secrets/tui).
5. The known carried concerns — verify each was either fixed or is tracked in docs/log.md: duplicate crypto in backup, S3 creds from env, git-leg chunking amendment deferred, backup canary command vs secrets add, restore's RECOVERY.md target-name coupling, tui includeDisabled choice.
Evidence = file:line + actual text. If something holds, say it holds.`,
    { label: 'adversarial-review', phase: 'Review', model: 'opus', schema: FINDINGS }),

  () => agent(`Fresh-clone verifier for exocomp M1. In a scratch dir under /tmp (never touching ${REPO}): git clone ${REPO} exocomp-clone; cd in; sh .agents/bootstrap.sh; make build; make check; then run the repo's own end-to-end gate: ./test/e2e/m1-gate.sh (it will start the existing lima instance 'exocomp-node0', run a vaulted playbook, back up, restore; allow up to 10 minutes; requires ansible-playbook/restic/age on PATH — export PATH="$HOME/.local/bin:$PATH" first). Report the REAL final line of the gate. Then: ./bin/exocomp version, doctor (all five tools should be found now), secrets list error-path on empty XDG_DATA_HOME, tui non-TTY error path. Cleanup the clone. Every deviation is a finding; report what actually happened.`,
    { label: 'fresh-clone-gate', phase: 'Review', model: 'sonnet', schema: FINDINGS }),
])

return { review, verify }