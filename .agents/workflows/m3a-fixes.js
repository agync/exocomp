export const meta = {
  name: 'exocomp-m3a-fixes',
  description: 'M3a maintenance fixes: sandbox gate regressions + security should-fixes',
  phases: [{ title: 'Fix', detail: 'single opus lane: 2 blockers + 2 should-fixes + 2 nits' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const SCHEMA = {
  type: 'object', required: ['commits','summary','verified','concerns'],
  properties: {
    commits: { type:'array', items:{ type:'object', required:['sha','title'], properties:{ sha:{type:'string'}, title:{type:'string'} } } },
    summary: { type:'string' }, verified: { type:'array', items:{type:'string'} }, concerns: { type:'array', items:{type:'string'} },
  },
}
phase('Fix')
const result = await agent(`Read <worktree>/AGENTS.md first. Worktree: ${REPO}/.worktrees/m3a-fix (branch m3a-fix/sandbox); you own the whole worktree. Conventions: logical-unit commits via "git -C <worktree>", conventional titles <= 52 chars, NO trailers, explicit staging, no slop comments, never sudo. Pre-commit runs make check (go test -race ./..., ~2 min) on the index — never bypass. Every fix gets a test that fails before it; reproduce blockers before fixing.

These come from a maintenance review that REPRODUCED both blockers on a fresh clone (the e2e gates are RED). Read the cited code before changing it.

BLOCKER 1 — sandboxed tofu cannot write its plan artifact (test/e2e/m2-gate.sh: "FAIL: plan expected exit 2, got 1", log: "Failed to write plan file .../data/exocomp/artifacts/tofu/...tfplan: permission denied"). Root cause: opArgv points tofu's -out= at dataDir/artifacts/tofu (cmd/exocomp/workspace.go ~660-678), but the local-sandboxed tier denies the data dir by design (internal/executor/sandbox.go Validate), so the job can't write there. DO NOT fix by granting the data-dir artifacts path to the sandbox — that punches a write hole into exactly what the sandbox exists to protect. CORRECT fix: have tofu write the plan into the job's OWN private/work dir (already granted, see job.grants()), then the DAEMON (unsandboxed, after the job returns) moves/seals that plan file into dataDir/artifacts via the existing plan-artifact sealing path (cmd/exocomp/planseal.go, internal/secrets/artifact.go, workspace.go recordPlan). Keep the plaintext plan only in the private dir (removed at job end) and the sealed copy in artifacts/. Verify the sandbox still denies the data dir (the existing sandbox denial tests must still pass) AND m2-gate.sh goes green.

BLOCKER 2 — sandboxed ansible refuses when the playbook/inventory sit in a dir that CONTAINS the data dir (test/e2e/m1-gate.sh: "grant ... covers .../data/exocomp, which no job may reach"). The guard failing safe is CORRECT (Landlock is allow-list; a read grant of a parent exposes the data-dir children — refusing is right). The bug is the TEST: m1-gate.sh writes play.yml/inventory.ini directly into $WORK while XDG_DATA_HOME=$WORK/data, artificially nesting source under the data dir's parent. FIX THE TEST: put play.yml/inventory.ini (and vaulted var) in a $WORK/src subdir that does NOT contain the data dir; keep the data dir at $WORK/data. Do the same defensive check in m2-gate.sh if it has the same shape. Also improve the guard's error to name the fix ("a source path may not be an ancestor of the data dir") — but do NOT weaken the refusal. Both gates must go green.

SHOULD-FIX 3 — manifest-MAC silent downgrade → git-target snapshot forgery. internal/backup/restore.go authenticateParts / authenticateSnapshotKey: when the .mac file is ABSENT, VerifyMACFile returns (false,nil) and restore only warns "legacy backup: integrity unverified" then proceeds. A git target is age-encrypted to PUBLIC recipients (confidentiality only, no origin authenticity), so the DEK-keyed manifest MAC is the ONLY control binding a git snapshot to the secret — an attacker with git write + the public recipients can forge a snapshot.db (attacker rule/schedule rows → shell action → post-restore RCE), self-consistent parts manifest, no .mac. Fix: once a DEK is in hand, a MISSING tag from a target that SHOULD authenticate is a REFUSAL, not a warning. Mark a target as authenticating (write a marker at first MAC'd backup, or record it on the target root), and refuse an untagged snapshot from a marked target with ErrManifestUnauthenticated; keep the loud legacy-warning path only behind an explicit --allow-unauthenticated restore flag. Restic targets (repo password = f(DEK)) are covered; the git path is the exposed one. Test: forge an untagged git snapshot at a marked target → restore refuses.

SHOULD-FIX 4 — ntfy public-topic refusal bypassable by case/trailing-dot. internal/notify/ntfy.go: publicNtfyHosts is checked as publicNtfyHosts[base.Hostname()] but url.Hostname() does not case-fold or strip a trailing dot ("NTFY.SH", "ntfy.sh." both reach the public server, neither matches). Fix: normalize — publicNtfyHosts[strings.ToLower(strings.TrimSuffix(base.Hostname(), "."))]; match any host under ntfy.sh. Test the variants.

NIT 5 — sandbox fail-open: an EXPLICIT --sandbox local-sandboxed request that can't be enforced degrades to unsandboxed with only a warning (cmd/exocomp/sandbox.go). Add fail-closed: an explicit TierLocalSandboxed request that probes Degraded REFUSES to run jobs (doctor/serve error), while TierAuto keeps warn-and-degrade. Test both.

NIT 6 — SandboxProfile.Validate compares lexically-cleaned (not symlink-resolved) grants vs the data dir (internal/executor/sandbox.go). EvalSymlinks both the data dir and each grant before covers(), or document grants must be pre-resolved. Test a symlinked-ancestor data dir.

Verification (REAL output required): make check; the before/after repro of both blockers; then BOTH gates from the worktree — ./test/e2e/m2-gate.sh then (PATH="$HOME/.local/bin:$PATH", lima node exocomp-node0 exists stopped) ./test/e2e/m1-gate.sh — paste their real final PASS lines. Host: go 1.26, tofu, ansible-playbook, restic, age, sops, podman, limactl, nono 0.74.0. Never sudo.`,
  { label:'fix:m3a', phase:'Fix', model:'opus', schema:SCHEMA })
return { result }