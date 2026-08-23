# Threat model & red-team findings

Second adversarial pass: 2026-08-23, against the agent-facing surface (write `/api/v1`,
remote CLI client, MCP server) as it was built out under ADR 0014. Findings are ids
`AI-1`..`AI-13`; the wave that closed them is [§ Agent-surface findings](#agent-surface-findings-ai-).

First adversarial pass: 2026-08-22, against v1.0.0 (tag, HEAD 5d77dce), five lenses
attacking a real freshly-built binary + data dir (see
`.agents/workflows/post-red-team.js`). Every "exploited" finding was reproduced against
a real binary, not read off the source. Ranked by realistic risk to a **solo homelab
operator** running one container/host, not theoretical CVSS.

exocomp is not deployed anywhere yet, so this is *fix-before-anyone-runs-it*, not incident
response. It is tracked here as the security posture of record and the blue-team backlog.

## Operator decisions

- **host.key at rest (finding E1).** TPM sealing is an **enhancement**, not the default —
  but **the default (non-TPM) story must be genuinely secure** (operator, 2026-08-22).
  Resolution: `host.key` is never a bare plaintext file. Default at-rest protection is
  **systemd-creds host-key wrapping** (`LoadCredentialEncrypted=`, `--with-key=host`) on
  the primary Quadlet/systemd tier — the unwrap secret is `/var/lib/systemd/credential.secret`
  (root-only, outside the exocomp data volume), so a stolen data volume / SSD snapshot does
  not contain it. Compose / non-systemd tier: **passphrase-wrapped** host.key (operator
  passphrase from an env/file outside the volume) with the residual risk stated honestly.
  TPM (`--with-key=host+tpm2`) is the hardware-binding enhancement, detected-and-verified,
  never claimed when absent. The false "TPM 2.0 detected — key sealed to this box"
  walkthrough text and the ADR 0003 "protects stolen disks" claim are corrected to the
  real posture as part of the fix.

## Findings

Severity is realistic solo-operator risk. Disposition: FIX (blue-team wave), HARDEN
(defense-in-depth), ACCEPT (documented residual), or DOC (honesty correction).

### HIGH

| id | finding | exploited | disposition |
|---|---|---|---|
| E1 | Evil-maid: `host.key` is plaintext, no TPM/systemd-creds sealing is implemented, yet the UI claims "key sealed to this box". A data-dir read yields every secret with `age -d` + stdlib GCM. | yes | **FIX + DOC** — secure default per the decision above; correct the UI/ADR now. |
| E2 | `identity migrate --drop-recipient` re-wraps the *same* DEK (no rotation); a dropped recipient with any retained `keyring.age` keeps decrypting everything, incl. secrets sealed after the drop. CLI says "dropped". | yes | **FIX** — rotate the DEK on drop (re-encrypt every row) or refuse and name rotation as the real revocation; reword output so it never implies revocation without rotation. |
| E6 | The E2 DEK rotation re-encrypts rows one at a time and flips `keyring.age` only after the loop finishes. A crash or mid-loop error leaves some rows under the new DEK and the rest under the old while `keyring.age` still wraps the OLD DEK — and nothing reads the staged `keyring.age.next` to reconcile, so the next `Open` unwraps the OLD DEK and the already-moved rows fail to decrypt. Silent data loss on the exact path revocation serves. | yes (test) | **FIX** — stage the new keyring/metadata before any row moves, promote atomically with `keyring.age` renamed last, and reconcile a detected `keyring.age.next` to completion on the next Open (load both DEKs, re-seal every row under the new one, idempotently). |
| S1 | Dependency resolution (`uv pip install`) runs **unsandboxed in the daemon context**, before the nono/Landlock job sandbox — a poisoned `pyproject.toml`/`requirements.txt` executes with daemon privileges. | analysis | **FIX** — run the resolver under the job sandbox tier; it is untrusted-source-derived code. |
| S2 | Signed-ref gate falls back to ambient host git config when `allowed_signers` is empty (the migration-0014 fallback); an attacker-signed ref was accepted that way. | yes | **FIX** — `--signed` fails closed with no exocomp-owned allowed-signers; never trust ambient git config. (Residual gap in the M3d story-1 fix.) |
| R1 | Dedup-anchor planting: an inert non-matching event becomes the dedup anchor, silently suppressing a later *actionable* alert. | yes | **FIX** — dedup keyed only on events that actually matched/fired, or scope the anchor. |
| B1 | Git restore leg: removing `keyring.age` from the backup ref downgrades the `AUTHENTICATED.mac` forgery-refusal to a warning; an attacker snapshot restores. (Known M3a residual — the marker-in-repo gap.) | yes | **FIX** — out-of-band authenticated marker (recovery-kit per-target authenticates flag). |

### MEDIUM

| id | finding | exploited | disposition |
|---|---|---|---|
| E3 | AEAD `name\|kind` AAD uses an unescaped `\|`; distinct (name,kind) rows collide and a ciphertext opens under the wrong identity — defeats the "move a ciphertext, it won't open" property. | yes | **FIX** — length-prefix / unambiguous AAD encoding. |
| D1 | Pre-auth argon2id memory-amplification DoS on the LAN web `/login` + bearer endpoints. | yes | **HARDEN** — per-source rate limit / connection cap before the KDF. |
| R2 | Resolve-event forgery withdraws a pending approval and cancels its remediation job — no approval credential, no matching rule. | yes | **FIX** — a resolve that cancels a remediation must be authenticated / rule-scoped. |
| R3 | Daily-cap flood self-disables a security-relevant rule, suppressing later real events until manual re-enable. | yes | **HARDEN** — a cap hit alerts loudly and/or does not silently disable a security rule. |
| S3 | Unsigned is the default; hostpath/volpath sources have no signature gate at all. | yes | **HARDEN + DOC** — surface the trust posture; a signed-source opt-in for local trees where it makes sense. |
| S4 | Python resolver: no index confinement, no hash pinning, pyproject deps passed as raw argv to uv (flag injection). | yes | **FIX** — confine the index, pin hashes where possible, pass deps past `--` / validate. |
| S5 | `known_hosts` pin (ADR 0010) is never applied to git source-sync — the forge host key is trust-on-first-use. | analysis | **HARDEN** — apply the pin to source-sync SSH. |

### LOW / INFO (defense-in-depth backlog)

- **E4** keyring-generation downgrade refusal trusts an unauthenticated root version file, no-ops on version 0 (and is moot until the DEK rotates — E2 fixes the root cause). HARDEN.
- **E5** DEK + subkeys live in ordinary swappable/core-dumpable heap despite the ADR's "mlocked memory" claim. FIX (mlock is cheap) or DOC.
- **D2** sandbox egress is filtered by port, not destination (a job can reach any host on 22/443). ACCEPT for v1 (documented) / HARDEN later with a destination allowlist.
- **D3** unauthenticated `/metrics` shares the state-backend listener, reachable by tofu-tier jobs. HARDEN — move or auth.
- **D4** `--hook-allow` CIDR allowlist is nullified behind a reverse proxy (X-Forwarded-For). DOC.
- **R4** a rule with no `--source` matches events from ANY hook (cross-source spoofing footgun). DOC + a lint/warn.
- **S6** pinned tools resolved by bare name on ambient PATH — no absolute pin/checksum. HARDEN.
- **U1** upgrade "verify" is a functional smoke test, not a staged-binary trust control (no signature/provenance). HARDEN — ties to the deferred cosign/SLSA supply-chain track.

## Blue-team waves

Grouped by domain, HIGH first. Each wave turns the red-team repro into a failing
regression test, fixes it, and proves the exploit is dead.

1. **Envelope — LANDED 2026-08-22** (commits 9cd6bb8/daca061/a7de445/88e23f1, merged
   015ab68): **E1 FIXED** (host.key resolved by precedence — systemd credential →
   scrypt-passphrase-wrapped → plaintext-with-LOUD-warning; init never writes a plaintext
   key when a passphrase is set; doctor reports the real tier; Quadlet
   `LoadCredentialEncrypted` + install.sh `harden-host-key`; the false "sealed to this box"
   walkthrough + ADR 0003 claims corrected; evil-maid repro now recovers nothing).
   **E2 FIXED** (`identity migrate --drop-recipient` rotates the DEK, re-encrypts every row,
   re-wraps to remaining recipients; a dropped recipient decrypts nothing pre- or post-drop).
   **E3 FIXED** (length-prefixed AAD, legacy fallback on open for pre-E3 rows). **E5 FIXED**
   (DEK mlock'd with lifecycle; doctor reports the real state). Residual follow-ups added to
   the backlog below.
   - **E2-resid** rotation is CLI-only + daemon-stopped, not a sequenced job with
     rollback/approval; derived consumers (restic repos, tofu state) are NOT auto-re-keyed
     (printed as manual follow-through). A daemon-sequenced rotation is a follow-up.
   - **E3-resid** un-migrated pre-E3 rows keep the legacy-AAD fallback, so the collision
     still exists for those; a one-time re-seal migration would close it fully.
   - **E1-resid** the systemd-credential tier is unit-tested on the read side only; the real
     `systemd-creds`/rootless-podman path needs validation on a real host.
   - **E4** (keyring-version downgrade on an unauthenticated file) — still open; the DEK now
     rotates (E2), reducing its blast radius.
   - **E6 FIXED — 2026-08-24** (dek-reseal lane): the E2 rotation is now crash-safe and
     resumable, closing a silent data-loss window on the revocation path. `RotateDEK` stages
     `keyring.age.next` + `recipients.next`/`keyring.version.next` (and `host.key.next` on a
     host rotation) *before* any row moves, re-encrypts every row under the new DEK, then
     promotes the staged files atomically with `keyring.age` renamed **last** — the single
     commit point. `secrets.Reconcile` detects a leftover `keyring.age.next` and finishes the
     interrupted migration on the next `Open`: it loads both DEKs (new from `keyring.age.next`,
     old from `keyring.age` while it still opens), re-seals every row under the new DEK
     decrypting each under whichever DEK holds it, and promotes — idempotent, so a crash
     *during* reconciliation converges on the next call. Wired into `serve` startup and the CLI
     rotate path; `RotateDEK` refuses to stack a fresh rotation on a pending one. The
     `name|kind` AAD and every E1..E5 property are preserved. Repro: a crash after N of M rows
     strands the moved rows on the old-DEK `Open` (failing test); after the fix every row opens
     under the new DEK, `keyring.age.next` is gone, and disabling the reconciliation kills the
     test (mutation-checked). Tests: `internal/secrets/reseal_crash_test.go`,
     `cmd/exocomp/reseal_recovery_test.go`.
     - **E6-resid** a transit-sealed store (auto-unseal, no host key on disk) with an
       interrupted reseal cannot reconcile without the host key: reconciliation resolves the
       DEKs through the age host-key path. The host-key path is the supported recovery; a
       transit-native reconcile is a follow-up. Owner: `internal/secrets`.
2. **Supply-chain / deps — S1/S2/S4/S5 LANDED 2026-08-22** (commits
   65690ce/8fa16f2/6a71495/532518f): **S1 FIXED** (the uv resolver runs under the job
   sandbox tier, not the daemon context — every uv invocation is wrapped in the nono
   launcher, granted its venv cache, the manifest, and HTTPS, and denied the data dir and
   the keys by the profile guard; a manifest whose install step reads a denied path fails
   closed, proven against the real launcher). **S2 FIXED** (a `--signed` sync refuses when
   there is no exocomp-owned allowed-signers rather than verifying against ambient git
   config; `source add --signed` refuses at add time without `--allowed-signers`/`--signer`;
   verification is pinned to ssh format — the attacker-signed-ref-via-ambient-config repro
   now refuses). **S4 FIXED** (a pyproject dependency specifier beginning with `-` is
   refused at parse, so `--index-url=…`/`-e /etc` cannot reach uv as a flag; accepted
   specifiers pass uv after `--`). **S5 FIXED** (a git `ssh://` source pins the forge host
   key from the recorded `known_host` store — strict verification when recorded, TOFU only
   for an unrecorded forge; a host-key mismatch fails the sync). **S3** (unsigned default /
   local-tree trust posture) is still open — HARDEN+DOC, not in this wave.
   - **S1-resid** the resolver profile's grant set (uv tool tree, base interpreter, uv
     cache) is validated against the sandbox by unit test with a stand-in uv; a live uv
     resolve under the sandbox on a real host — fetching a real index, seeding a real
     interpreter — still needs on-host validation, like the E1 systemd-credential tier.
   - **S5-resid** the pin consumes the `known_host` store but does not itself record the
     forge key; recording is `host onboard`/`ssh-keyscan`, so a forge's first sync is still
     TOFU until its key is recorded. A non-standard forge SSH port needs its `known_host`
     row keyed as `[host]:port`.
3. **Reactive / approval — R1/R2/R3 LANDED 2026-08-22** (commits
   d0884b9/167f9d1/e719346): **R1 FIXED** (the dedup window anchors only on events
   that actually matched a rule and acted — `matched`/`gated`/`fired` — so an inert
   non-matching event planted first can no longer become the anchor that suppresses a
   later real alert sharing the key; legitimate dedup of a genuine repeat is preserved,
   proven against the engine). **R2 FIXED** (a resolve withdraws an approval only when
   it is scoped to the rule that raised it: same source, admitted by that rule's source
   filter, and matching the rule when its status is read as firing; a forged resolve
   that shares only the dedup key — stripped of the labels the rule fired on — matches
   no rule and cancels nothing, and the approval then fails closed at its TTL). **R3
   FIXED** (a daily cap throttles rather than disables: the action is withheld but the
   rule stays armed, keeps evaluating so every over-cap event stays visible as a
   suppressed row naming the rule, recovers on its own when the rolling window clears,
   and pages at max priority once per window so a flood cannot blind a security rule
   into a silent disabled state — the R3 exploit — nor storm the operator's phone). R4
   (source-less rule cross-source spoofing) is DOC-only and still open.
   - **R2-resid** a legitimate resolve is not auto-withdrawn if the rule that raised the
     approval was deleted, disabled-to-not-compiling, or is otherwise unresolvable at
     resolve time; the approval still fails closed at its TTL, and the operator can
     reject it by hand. This is the safe direction (a resolve that cannot be scoped
     cancels nothing) but means a rule deleted mid-approval loses the auto-cancel.
   - **R3-resid** the once-per-window cap page is process state (like the approval
     re-publish), so a daemon restart re-pages a still-capped rule once; louder than
     silent, but not a swept ledger. Stale "cap disables the rule" wording remains in
     `cmd/exocomp/rule.go` help, `internal/store/rule.go`'s `SetRuleEnabled` comment,
     and `internal/notify/notify.go` — outside the reactive lane's owned files, left
     for the integrator to correct.
4. **Daemon / backup — B1/D1/D3 LANDED 2026-08-22** (commits 8073781/9e2c841/a90a3e4,
   merged a90a3e4; CLI wiring + doctor follow-up on main): **B1 FIXED** (a per-target
   `Authenticates` fact carried out-of-band on the recovery kit and honored at restore —
   a git snapshot whose in-repo tag + `AUTHENTICATED.mac` were stripped is refused as a
   downgrade, not trusted as a legacy target; `--allow-unauthenticated` is still the loud
   escape; `--target-authenticates` on `restore` + the kit's section-6 record wire it
   end-to-end). **D1 FIXED** (a KDF concurrency gate — global ceiling + per-source in-flight
   cap — sheds a pre-auth flood with 503 having run no argon2id; a real login still 200s).
   **D3 FIXED** (`/metrics` bearer-gated with an envelope-derived scrape token, constant-time,
   fails closed with no deriver; `doctor` now presents the token).
   - **D1-resid** the per-source cap keys on RemoteAddr (X-Forwarded-For, D4-adjacent) — the
     global ceiling still bounds total KDF memory behind a proxy.
   - **B1-resid** the kit marks every target `Authenticates`; a genuinely-legacy target
     needs `--allow-unauthenticated`. The scrape/session derive purposes are operational
     (not on the recovery card) — add to secrets.Purposes() if card-reproducibility is wanted.

5. **Agent surface — AI-1..AI-13 LANDED 2026-08-23** (branch `redteam-api`): every exploited
   finding of the second pass closed, HIGH first, each one a failing-then-passing regression
   test. See [§ Agent-surface findings](#agent-surface-findings-ai-) for the table and the
   residuals.

**All six exploited HIGH findings of the first pass (E1, E2, S1, S2, R1, B1) are now closed,
as are all five of the second pass (AI-1..AI-5).** Remaining: medium/low defense-in-depth
(S3, D2, D4, R4, S6, E4, U1) + the residuals noted above and in the agent-surface section.

Each wave is a build+verify workflow that reproduces the finding as a failing test first,
then fixes it, then proves the exploit no longer works (red-team's repro becomes a
regression test). The red-team re-runs against each wave.

## Agent-surface findings (AI-\*)

Second pass, 2026-08-23, against the three programmatic faces ADR 0014 ratifies: the write
half of `/api/v1`, `exocomp login` + `--remote`, and the stdio MCP server. Every finding
below was reproduced against a real binary or the real handler; every one marked
**exploited-then-closed** now has that repro as a failing-then-passing regression test.

### HIGH — all six closed

| id | finding | exploited | disposition |
|---|---|---|---|
| AI-1 | MCP stdio has no credential and no scope, yet advertised `approve_job`/`reject_job` beside `workspace_apply` — an agent gated its own destructive job and then approved it. Prompt injection amplifies it: `get_job_events`/`list_events` return attacker-posted webhook text to the same session that holds the release. | yes | **FIXED — exploited-then-closed.** The decision tools are gone from the stdio face; a gated job is decided from the browser/phone surface, the CLI, or an approve-scope `/api/v1` token. They return with a token-carrying HTTP/SSE transport (ADR 0014 §3's scope-gated visibility). |
| AI-14 (2026-08-24, branch `composed-approval`) | A `--require-approval` step in a COMPOSED CHAIN was not durably gated mid-run: the Sequencer's `runStep` released an `awaiting_approval` workflow step unconditionally, and `ClaimJob` had no approval join, so a running daemon released and ran a gated step with no operator decision — the composed-chain twin of AI-1/AI-2, defeating the same gate a single apply passes. | yes | **FIXED — exploited-then-closed.** A gated step is marked `require_approval` in the ledger; the Sequencer arms a pending approval (the same `CreateApproval`/`DecideApproval` machinery a single gated apply uses) and parks the step in `awaiting_approval` instead of releasing it — approve releases exactly that step, reject cancels it and blocks downstream. `ClaimJob` now refuses any queued job carrying a pending approval, so the gate holds at the claim however a job reached queued. Both guards mutation-checked (`internal/queue/workflow_approval_test.go`, `internal/store/claim_approval_test.go`). |
| AI-2 | `operator` satisfied both W and A: the only scope that can enqueue a gated apply could also settle its approval, so handing an agent the token it needs to plan/apply handed it the authority the gate exists to withhold. | yes | **FIXED — exploited-then-closed.** `/api/v1`'s decision route takes `approve` alone, and BOTH decision paths refuse a caller the gated job's ledger records as its actor. The browser surface keeps operator-or-approve for the human at the console. |
| AI-3 | TOCTOU between `CreateArtifact` and the verdict event: an apply-by-id landing in the window read "no verdict" as "no policy" and enqueued plainly against a deny-all workspace. | yes | **FIXED — exploited-then-closed.** A plan on a workspace with enabled policies and no adjudication record is unconsumable on every face; the seal records a no-match adjudication so a legitimately unmatched plan still applies; and the daemon evaluates policy BEFORE it seals, so the remaining window is one event append. |
| AI-4 | A fresh apply consulted no policy at all — omitting `plan` skipped the whole deny/require_approval machinery, so `policy add --action deny` protected only the plan→apply-by-id path. | yes | **FIXED — exploited-then-closed.** A fresh apply on a workspace with enabled plan policies is refused in the service verb, so every face inherits it; plan-then-apply-by-id is the adjudicable path. |
| AI-5 | `--assume-yes` is a root persistent flag and the only way to pin at all on non-interactive stdin, and it also overrode the changed-fingerprint refusal — so the exact invocation every script/agent must use re-pinned to whatever certificate answered and handed it the bearer token. | yes | **FIXED — exploited-then-closed.** Replacing a CHANGED fingerprint, or re-pointing an alias at another endpoint, takes the login-only `--replace-pin`; `-y` alone refuses non-zero and leaves `hosts.toml` untouched. |

### MEDIUM — all four closed

| id | finding | exploited | disposition |
|---|---|---|---|
| AI-6 | `GET /api/v1/health` let a READONLY token fork eight PATH-resolved subprocesses and unwrap the keyring per request, at any rate — outside the one job queue and the KDF gate. | yes | **FIXED — exploited-then-closed.** The summary is served from a 10s cache whose refresh is serialized, so a flood of reads costs one probe. The bare-name PATH resolution and the per-probe `secrets.Open` live in `internal/health` and remain open (S6-adjacent) — see the residuals. |
| AI-7 | `Idempotency-Key` was one global namespace: a planted key returned another token's job for a write that never enqueued, and wrote the caller's attribution onto a foreign ledger. | yes | **FIXED — exploited-then-closed.** Keys are namespaced by face, authenticated token id, and route before they reach the queue, which also puts the queue's reserved `wf:` step keys out of reach of any face. |
| AI-8 | Cookie sessions were accepted on `/api/v1` WRITE routes with no CSRF token, no Origin/Referer check and no Content-Type check. SameSite=Strict is the whole mitigation, and it does not scope to a port. | yes | **FIXED — exploited-then-closed.** Write routes require an explicit bearer credential; reads still resolve a session, and the browser's own same-origin decide form is unchanged. |
| AI-9 | A saved plan applied by id with no check that it belonged to the workspace in the path — one workspace's sealed plan ran in another, under that workspace's backend bearer and state passphrase, with the stale-plan serial read from a workspace the plan was never made against. | yes | **FIXED — exploited-then-closed.** The plan's owner is read back from its plan job's runbook and a mismatch is an `invalid_argument` before anything is enqueued or decrypted. |

### LOW — all four closed

| id | finding | exploited | disposition |
|---|---|---|---|
| AI-10 | A revoked token kept receiving an already-open SSE job stream: auth was resolved once, at attach. | yes | **FIXED — exploited-then-closed.** The tail re-resolves its token row on every poll (a row read, no KDF) and ends the stream when it stops authenticating, with a 12h hard lifetime as backstop. |
| AI-11 | No rate limit, lockout or failure counter on bearer auth (the KDF gate caps parallelism, not rate), and the early return for an unknown/inactive row was a 35× timing oracle for which token ids exist. | yes | **FIXED (rate half) + ACCEPTED (timing half).** Consecutive failures back off per source and per token row (429 + `Retry-After`, no KDF spent) and are logged once per run — this is the guard that bounds guessing and it stays. The timing equalizer (a decoy argon2id paid on the absent/inactive path so it matched a live row's latency) was **REMOVED** (simplification S1): it never held — a live row hashes at `argonTime=1` while the decoy PHC hardcoded `t=3`, so a ~3× gap survived (review finding #6) — and the side-channel it aimed at is out of the threat model for a single-operator homelab. Reasoning: the secret is a 128-bit bearer token, so the oracle only leaks which row _ids_ exist (small integers already implied by the id-in-the-clear scheme), not any secret bytes; and turning that leak into enumeration requires a guessing run, which the fail counter + KDF gate refuse **before the KDF runs** regardless of whether the id is live or absent (`TestGuessingRunRefusedBeforeKDF`). The verify path is now: look up the row; absent, inactive, or secret-mismatch all return unauthenticated with no decoy KDF; the constant-time digest compare on a live row is unchanged. |
| AI-12 | A `--source` playbook escaped the checkout (`../`, or an absolute path that was not rebased at all), so a run recorded against a signed source executed a tree the signature never covered. | yes | **FIXED — exploited-then-closed.** The joined path is resolved (symlinks included) and required to stay inside the checkout, an absolute playbook with `--source` is refused, and a source name is charset-validated before it is spliced into any path. |
| AI-13 | MCP writes were wholly unattributed — an agent's write read off the ledger as the operator's own CLI run — and a `tools/call` sent as a JSON-RPC notification mutated state while writing zero bytes back. | yes | **FIXED — exploited-then-closed.** Writes record the `mcp` face, and a side-effecting method sent without an id is refused with `invalid_request` instead of executed. |

### Residuals from this wave

- **AI-1-resid — CLOSED.** The stdio face itself is gone: `exocomp mcp` and the bespoke
  stdio server were removed entirely (ADR 0014 §3), not just their decision tools. The one
  remaining MCP surface is MCP-over-HTTP (`POST /mcp`), which authenticates with an ADR-0009
  bearer token, scope-gates `tools/list` and every `tools/call`, and re-checks the token on
  every call so a revoke bites on the next request — the token-scoped transport this residual
  called for is what shipped, closing it structurally rather than leaving it bounded.
- **AI-2-resid** an operator driving a remote daemon needs an `approve`-scope token for
  `approval decide --remote`; the operator token no longer settles approvals over `/api/v1`.
  The self-approval refusal keys on the token NAME recorded in the ledger, so a revoked
  name later reused by a new token would be treated as the same principal.
- **AI-3-resid** the seal and the verdict are still two writes, not one transaction: the
  window is now one `AppendJobEvent` wide and fail-closed covers it, but a store-level
  transaction spanning `CreateArtifact` + the verdict event would close it structurally.
  That needs the store seam, which this lane does not own.
- **AI-6-resid** the fixes here are in `internal/web`. The probe itself still resolves seven
  optional tools by BARE NAME on the ambient PATH (finding S6) and still calls
  `secrets.Open(dataDir)` per run to mint the `/metrics` scrape token; deriving that once at
  daemon start and pinning tool paths belong to `internal/health`.
- **AI-7-resid** the recovered job's runbook/kind/argv are still not compared against the
  request (`queue` is not this lane's file); with the namespace in place a recovered job can
  only be the same principal's own earlier call to the same verb, so the mismatch case is a
  retry with different arguments, which still returns the first job.
- **AI-11-resid** the backoff is process state and per-source keys on `RemoteAddr` (D1/D4
  adjacent: a proxy collapses sources). Failures are logged, not recorded on the token row,
  so a guessing run is visible in the log but not in `token list`. The absent-vs-live auth
  timing gap is accepted, not closed (see the AI-11 row): the equalizer that tried to mask it
  is gone because it did not hold and the leak (which row ids exist) is not secret material a
  128-bit token defends. `internal/web/auth.go` owns the verify path.

## Optional OpenBao Transit seal (ADR 0013)

An operator may move the DEK's wrapping key off the data volume into an OpenBao Transit key
(the auto-unseal pattern). It is **additive**: `keyring.transit` wraps the DEK via
`transit/encrypt` beside an untouched `keyring.age`, so the printed recovery card still
opens the store **without OpenBao** — recovery never depends on the vendor (invariant 7).

- **Threat addressed.** A stolen data volume with the seal enabled contains no key that
  unwraps the DEK: the host key need not be on disk, and the transit key never leaves
  OpenBao. This closes the stolen-disk gap that a plaintext `host.key` leaves open, for the
  daemon's normal boot path.
- **Failure mode is explicit.** A configured-but-unreachable OpenBao fails `Open` with a
  clear error and never silently falls back to the host-key path — an outage cannot quietly
  downgrade the chosen seal. Reverting is a deliberate `exocomp seal disable`.
- **New trust.** OpenBao can read the DEK it wraps, and its own seal/audit posture joins the
  operator's threat model. The token authenticating to OpenBao is sourced from the
  environment or a file outside the data dir and is never written into it; neither the token
  nor the DEK plaintext ever reaches a log line or job event.
- **Not a defence against root** on the box, same as ADR 0003.
