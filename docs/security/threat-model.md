# Threat model & red-team findings

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

**All six exploited HIGH findings (E1, E2, S1, S2, R1, B1) are now closed.** Remaining:
medium/low defense-in-depth (S3, D2, D4, R4, S6, E4, U1) + the residuals noted above.

Each wave is a build+verify workflow that reproduces the finding as a failing test first,
then fixes it, then proves the exploit no longer works (red-team's repro becomes a
regression test). The red-team re-runs against each wave.
