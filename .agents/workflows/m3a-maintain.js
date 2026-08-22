export const meta = {
  name: 'exocomp-m3a-maintain',
  description: 'M3a maintenance: adversarial review + fresh-clone gate verification',
  phases: [{ title: 'Review', detail: 'adversarial (opus) + fresh-clone gates (sonnet)' }],
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
          file: { type: 'string' }, claim: { type: 'string' },
          evidence: { type: 'string' }, suggested_fix: { type: 'string' },
        },
      },
    },
  },
}

phase('Review')
const [review, verify] = await parallel([
  () => agent(`Adversarial reviewer for exocomp at ${REPO}, milestone M3a: reactive engine (webhook→cel-go→guardrail ledgers→approval gate→ntfy), nono local-sandboxed tier, security folds (keyring versioning, manifest HMAC, kit derive/verify, plan-artifact encryption, DCO). Read AGENTS.md (rubric), .agents/workflows/m1-plan.md M3 section, and docs/adr/0005 (sandbox), docs/architecture/reactive.md, docs/operations/reactive.md. Read-only: modify/commit NOTHING.

Attack the highest-risk new surfaces — this milestone added the security-critical reactive path and a sandbox whose whole point is denial:
1. THE GATE: can a job reach 'running' without its approval? Trace queue/gate.go EnqueueGated + store.CreateGatedJob + the two claim layers. Can a scheduled tofu-plan-that-drifted, a webhook retry, or a resolve-race slip a destructive action past the gate? Is the approval token truly single-use + constant-time + fail-closed on expiry (check the sweep actually rejects, not just marks)?
2. THE SANDBOX: does SandboxProfile.Validate actually deny the data dir on EVERY path (symlink into it? relative grant? the run/ tmpdir that lives inside the data dir)? The lane itself FALSIFIED an ADR claim — pathname AF_UNIX sockets aren't Landlock-mediated. Given that, can a sandboxed job reach the state-backend on 127.0.0.1 or a unix socket to exfiltrate? Is --block-net's exception set (ssh:22, https:443, loopback) exploitable? Does probe-failure REALLY degrade loud, or can a missing nono silently downgrade?
3. SECRETS/CRYPTO: manifest HMAC — is the MAC over the right bytes (canonical? does reordering/truncating parts still authenticate)? keyring downgrade — can a v1 keyring at ONE target root among several be used to force a downgrade restore? plan-artifact encryption — is the plaintext plan ever written to disk before sealing, or left in a tmp? Does the redactor cover the NEW event paths (reactive ingress raw bodies, ntfy publish payloads, approval reasons)?
4. NTFY/NOTIFY: the public-topic refusal — bypassable with a self-hosted-looking URL that's actually public? Are decision-link URLs ever logged or in an event? CEL: sandboxed enough — can an expr exhaust CPU/memory past the budget, or read something it shouldn't?
5. CONTRACTS: docs vs the code that shipped; the approval-purpose reconciliation (card said approval-token, engine uses approval — is it consistent everywhere now?); migration 0005 forward-only + reversible-by-restore.
Evidence = file:line + real text. If a guard holds, say it holds and how you proved it.`,
    { label: 'adversarial-review', phase: 'Review', model: 'opus', schema: FINDINGS }),

  () => agent(`Fresh-clone verifier for exocomp M3a. In a scratch dir under /tmp (never touch ${REPO}): git clone ${REPO} exocomp-clone; cd in; sh .agents/bootstrap.sh; make build; make check (this now runs go test -race ./... — full suite; allow time). export PATH="$HOME/.local/bin:$PATH". Then run BOTH e2e gates from the clone and report their REAL final lines: ./test/e2e/m2-gate.sh first (fast), then ./test/e2e/m1-gate.sh (starts the stopped lima instance exocomp-node0; ~10 min). Then exercise the NEW M3a surfaces end-to-end against a scratch XDG_DATA_HOME with a running daemon (nono 0.74 is on PATH): (a) exocomp rule add with a CEL expr, exocomp serve, curl an alertmanager-shaped payload at the printed hook URL, confirm it gates (exocomp approval list shows pending), decide via the token URL, confirm the job runs; (b) exocomp doctor shows the sandbox tier + nono version + landlock ABI; (c) exocomp kit derive restic/<name> matches an actual restic repo password; (d) a global silence swallows an event. Report exactly what happened for each; every deviation is a finding. Clean up (remove clone, stop lima node).`,
    { label: 'fresh-clone-gates', phase: 'Review', model: 'sonnet', schema: FINDINGS }),
])

return { review, verify }