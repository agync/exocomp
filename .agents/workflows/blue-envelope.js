export const meta = {
  name: 'exocomp-blue-envelope',
  description: 'Blue-team wave 1: fix the HIGH envelope red-team findings (E1/E2/E3/E5)',
  phases: [{ title: 'Harden', detail: 'envelope (opus, high effort)' }],
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
phase('Harden')
const results = await parallel([
  () => agent(`Read <worktree>/AGENTS.md, docs/security/threat-model.md (findings E1/E2/E3/E5 and the OPERATOR DECISION on host.key), docs/adr/0003-secrets-envelope.md FIRST.
YOUR LANE: blue-envelope. Worktree: ${WT}/blue-envelope (branch blue-envelope; all git via "git -C ${WT}/blue-envelope"). This is a SECURITY HARDENING pass fixing HIGH red-team findings against the age->DEK envelope. Own EXCLUSIVELY: internal/secrets/**, cmd/exocomp/{init.go,setup.go,kit.go} and the identity/recipient CLI (whichever file holds \`identity migrate\`), packaging/** (Quadlet unit + install.sh for the systemd-creds path), docs/adr/0003-secrets-envelope.md, docs/design/first-run-walkthrough.html (correct the false claim). Do NOT touch internal/{source,executor,reactive,backup,queue}, cmd/exocomp/{handoff,host,provision,source}.go, internal/store (except a migration you own for E3 re-seal if needed) — other lanes / the running A+B feature.

METHOD (required): for EACH finding, FIRST write a test that reproduces the red-team exploit (it should FAIL against current code — i.e. prove the attack works), THEN fix, THEN show the same test now proves the attack FAILS. The red-team repro becomes a regression test. Conventions: logical-unit commits per finding; conventional titles <=52 chars; NO trailers; explicit staging; never sudo; the pre-commit hook runs the FULL make check (~2.5 min, and note two tests — TestTwoProcessesSerializeAConcurrencyKey, TestServeShutdownRetiresWhatItCannotFinish — are LOAD-FLAKY; if one fails, re-run rather than assuming a regression).

FIXES (priority order):
1. **E1 — secure-default host.key at rest + honest docs (HIGH).** Per the operator decision: TPM is an ENHANCEMENT; the DEFAULT (non-TPM) must be genuinely secure, i.e. a data-dir/volume read alone must NOT yield host.key. Implement: (a) the daemon loads host.key from a systemd credential when \`$CREDENTIALS_DIRECTORY/host.key\` exists (systemd-creds \`LoadCredentialEncrypted\`, \`--with-key=host\` — the unwrap secret is /var/lib/systemd/credential.secret, OUTSIDE the exocomp data volume), and the Quadlet unit + install.sh set this up; (b) a passphrase-wrapped host.key path for the compose/non-systemd tier (an operator passphrase from \`EXOCOMP_HOST_KEY_PASSPHRASE\` or a file outside the data dir; use age scrypt or equivalent) so the key at rest is not plaintext; (c) TPM \`--with-key=host+tpm2\` as a detected-and-verified enhancement, never claimed when absent; (d) init/doctor report the ACTUAL protection state honestly (plaintext-in-datadir=LOUD WARNING with the fix, vs systemd-cred / passphrase / tpm), and init refuses/warns loudly if it would leave a bare plaintext key on the primary path; (e) CORRECT docs/design/first-run-walkthrough.html ("TPM 2.0 detected - key sealed to this box") and the ADR 0003 "protects stolen disks" claim to the real posture. REGRESSION TEST: the evil-maid repro (copy the data dir, recover the DEK with age+GCM) must now FAIL on the protected default (no plaintext host.key to read).
2. **E2 — revocation actually revokes (HIGH).** \`identity migrate --drop-recipient\` must ROTATE the DEK: generate a new DEK, re-encrypt every sealed row + re-derive subkey consumers, re-wrap the keyring to the REMAINING recipients only, bump the generation, and re-key what depends on the DEK (document the restic-repo/tofu-state re-key follow-through). A dropped recipient's retained old keyring.age must then unwrap only the OLD, now-unused DEK and decrypt NOTHING sealed after (they are under the new DEK). If a full rotation is too large to complete safely in one step, at minimum REFUSE to present a drop as done — reword output so it never implies revocation without rotation and name rotation as the real step; but prefer implementing the rotation. REGRESSION TEST: the red-team repro (dropped recipient decrypts a post-drop secret) must now FAIL.
3. **E3 — unambiguous AEAD AAD (MEDIUM).** Replace \`name+"|"+kind\` (envelope.go:322) with an unambiguous encoding (length-prefixed, e.g. len(name)||name||len(kind)||kind) so distinct (name,kind) rows cannot collide. THIS CHANGES THE AAD and breaks existing ciphertexts — handle migration: version the AAD and re-seal existing rows to the new AAD on a one-time migration (own the migration), or detect-and-reseal on open. REGRESSION TEST: the collision repro (open one row's ciphertext under another row's identity) must now FAIL, and existing sealed rows still open after migration.
4. **E5 — mlock the DEK (LOW/INFO).** mlock the DEK + derived subkey buffers (syscall/unix Mlock) with a graceful fallback where unavailable, and have doctor report the real state; OR if mlock is not reliably available, correct the ADR's "mlocked memory" claim. Cheap; do it if feasible.

GATE (paste REAL output): each finding's before(exploit works)/after(exploit fails) test; existing secrets tests still green (backward-compat for E3); doctor honest protection-state output; the corrected walkthrough/ADR text. Full make check green. Report with the SCHEMA. Do NOT touch other lanes' files.`,
    { label: 'lane:envelope', phase: 'Harden', model: 'opus', effort: 'high', schema: SCHEMA }),
])
return { lanes: results.filter(Boolean) }
