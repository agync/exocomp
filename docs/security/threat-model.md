# Threat model

exocomp's security posture of record: the assets it protects, the trust boundaries it
draws, the adversaries and threats it considers, the mitigations in place for each, and the
residuals it knowingly accepts.

Threats are ranked by realistic risk to a **solo homelab operator** running one container on
one host, not by theoretical CVSS. The threat catalog below uses stable ids (`E`/`S`/`R`/`B`/`D`/`AI`)
as an index into families of related threats; each catalogued threat names the mitigation
that stands against it today, and each mitigation is pinned by a regression test.

## Assets

- **The secret envelope.** `host.key`, the data-encryption key (DEK) it wraps, and the
  per-row secrets the DEK seals. Compromise yields every managed credential.
- **Source integrity.** The playbook and tofu trees a run executes, and the git refs and
  Python dependency manifests they are resolved from.
- **The approval gate.** The decision that a destructive apply, or a security-relevant
  reactive rule's action, waits on. Its integrity is what keeps an automated or delegated
  action from running unbid.
- **Backups and the recovery kit.** The restic repos, git bundles, and the printed recovery
  card that reconstructs the store without this project's code or any vendor.
- **The programmatic surface.** The write half of `/api/v1`, the remote CLI client, and the
  MCP-over-HTTP server — the faces a delegated agent or script drives, each behind a scoped
  bearer token.

## Trust boundaries

- **Data volume vs. host root.** The at-rest data dir is assumed reachable by an attacker who
  is *not* root on the live host (a stolen SSD, a volume snapshot, an evil-maid read). Root on
  the running box is explicitly **out of scope** — the same boundary ADR 0003 draws.
- **Daemon context vs. job sandbox.** Untrusted-source-derived code (dependency resolution,
  playbooks, tofu) runs under the nono/Landlock job sandbox tier, never in the daemon's own
  privilege.
- **LAN/tailnet web surface vs. the operator.** The web and `/api/v1` listeners are reachable
  on the LAN or tailnet before authentication; only the operator's credential crosses into
  authenticated state.
- **Scoped token vs. its scope.** A delegated agent or script holds a bearer token scoped to
  specific verbs. The boundary is that a token cannot exceed its scope — most sharply, that
  the scope which enqueues a gated apply cannot also settle the gate.
- **External trust.** Forge SSH host keys, git allowed-signers, and (optionally) an OpenBao
  Transit key sit outside the box; each is trusted only under an explicit, verifiable pin.

## Adversaries considered

- An attacker who obtains the data volume at rest but not host root.
- An attacker who can post webhook or event text the operator (or a delegated agent) later
  reads — the prompt-injection vector into an agent session that also holds a release.
- An attacker who controls a source: a poisoned dependency manifest, an unsigned or
  wrong-signed git ref, a playbook path that escapes its checkout.
- An unauthenticated LAN/tailnet peer hitting the web and API endpoints, including for
  resource-exhaustion (KDF amplification) rather than data theft.
- A delegated token-holder trying to exceed its scope: self-approving a gated job, applying
  across workspaces, or re-pinning a login to an endpoint of its choosing.

Out of scope: root on the running host; side-channel timing attacks against opaque 128-bit
token ids (see AI-11).

## Envelope (E)

The DEK is wrapped by `host.key` and seals every per-row secret.

| id | threat | mitigation in place |
|---|---|---|
| E1 | Evil-maid / stolen data volume: a plaintext `host.key` in the data dir yields every secret with `age -d` + stdlib GCM. | `host.key` is never a bare plaintext file by default. It resolves by precedence: systemd-credential host-key wrapping (`LoadCredentialEncrypted=`, `--with-key=host`) on the Quadlet/systemd tier — the unwrap secret is `/var/lib/systemd/credential.secret`, root-only and outside the data volume, so a stolen volume lacks it; scrypt-passphrase-wrapped on the compose/non-systemd tier, the passphrase sourced from env/file outside the volume; plaintext only with a loud `doctor` warning and never written by init when a passphrase is set. TPM binding (`--with-key=host+tpm2`) is a hardware enhancement, detected and verified, never claimed when absent. `doctor` reports the real tier. |
| E2 | `identity migrate --drop-recipient` re-wrapping the *same* DEK would let a dropped recipient holding any retained `keyring.age` keep decrypting everything, including secrets sealed after the drop. | The drop rotates the DEK: every row is re-encrypted and re-wrapped only to the remaining recipients, so a dropped recipient decrypts nothing before or after the drop. |
| E3 | AEAD `name\|kind` AAD with an unescaped separator lets distinct (name, kind) rows collide, so a ciphertext opens under the wrong identity — defeating the "move a ciphertext and it won't open" property. | AAD is length-prefixed so (name, kind) encodes unambiguously; a legacy-encoding fallback on open reads rows sealed before the change. |
| E5 | The DEK and subkeys living in ordinary swappable, core-dumpable heap could leak through swap or a core dump. | The DEK is `mlock`'d with a managed lifecycle; `doctor` reports its real state. |
| E6 | The E2 rotation re-encrypting rows one at a time and flipping `keyring.age` only at the end leaves a crash mid-loop with some rows under the new DEK and the rest under the old, while `keyring.age` still wraps the old — silent data loss on the exact path revocation serves. | `RotateDEK` stages the new keyring and metadata (`keyring.age.next`, `recipients.next`/`keyring.version.next`, and `host.key.next` on a host rotation) *before* any row moves, re-encrypts every row under the new DEK, then promotes atomically with `keyring.age` renamed **last** — the single commit point. `secrets.Reconcile` detects a leftover `keyring.age.next` on the next `Open`, loads both DEKs, re-seals every row under the new one (decrypting each under whichever DEK holds it), and promotes — idempotent, so a crash during reconciliation converges on the next call. `RotateDEK` refuses to stack a fresh rotation on a pending one. All E1–E5 properties are preserved across the rotation. |

**Accepted residuals.**

- **E1 —** the systemd-credential tier is unit-tested on the read side; the live
  `systemd-creds` / rootless-podman path still needs validation on a real host.
- **E2 —** rotation is CLI-only and daemon-stopped, not a sequenced job with
  rollback/approval, and derived consumers (restic repos, tofu state) are not auto-re-keyed
  (they are printed as manual follow-through). A daemon-sequenced rotation is future work.
- **E3 —** rows sealed under the legacy AAD keep the fallback on open, so the collision
  persists for un-migrated rows until a one-time re-seal migration runs.
- **E4 —** the keyring-generation downgrade refusal trusts an unauthenticated root version
  file and no-ops on version 0. Accepted: the DEK now rotates (E2), which bounds the blast
  radius. Authenticating the version file is future hardening.
- **E6 —** a transit-sealed store (auto-unseal, no host key on disk) whose reseal is
  interrupted cannot reconcile without the host key, because reconciliation resolves the DEKs
  through the age host-key path. The host-key path is the supported recovery; a transit-native
  reconcile is future work.

## Sources and supply chain (S)

Playbooks, tofu, and their dependencies are untrusted-source-derived until proven otherwise.

| id | threat | mitigation in place |
|---|---|---|
| S1 | Dependency resolution (`uv pip install`) running in the daemon context, before the job sandbox, lets a poisoned `pyproject.toml`/`requirements.txt` execute with daemon privileges. | The resolver runs under the job sandbox tier: every `uv` invocation is wrapped in the nono launcher, granted its venv cache, the manifest, and HTTPS, and denied the data dir and keys by the profile guard. A manifest whose install step reads a denied path fails closed. |
| S2 | A `--signed` ref gate falling back to ambient host git config when `allowed_signers` is empty accepts an attacker-signed ref. | `--signed` fails closed with no exocomp-owned allowed-signers and never consults ambient git config; `source add --signed` refuses at add time without `--allowed-signers`/`--signer`; verification is pinned to ssh signature format. |
| S4 | A pyproject dependency specifier passed as raw argv to `uv` allows flag injection (`--index-url=…`, `-e /etc`). | A specifier beginning with `-` is refused at parse; accepted specifiers reach `uv` only after `--`. |
| S5 | Git source-sync using trust-on-first-use for the forge host key (the `known_hosts` pin of ADR 0010 never applied) allows a first-sync MITM. | An `ssh://` source pins the forge host key from the recorded `known_host` store: strict verification when the key is recorded, a host-key mismatch fails the sync. |

**Accepted residuals.**

- **S1 —** the resolver profile's grant set is validated against the sandbox by unit test
  with a stand-in `uv`; a live `uv` resolve under the sandbox on a real host — fetching a real
  index, seeding a real interpreter — still needs on-host validation.
- **S3 —** unsigned is the default; path sources have no signature gate. Accepted
  for now with the trust posture documented; a signed-source opt-in for local trees is future
  hardening.
- **S5 —** the pin consumes the `known_host` store but does not itself record the forge key
  (recording is `host onboard` / `ssh-keyscan`), so a forge's first sync is TOFU until its key
  is recorded; a non-standard forge SSH port needs its `known_host` row keyed as `[host]:port`.
- **S6 —** pinned tools are resolved by bare name on the ambient PATH, with no absolute pin or
  checksum. Future hardening.

## Reactive engine and approvals (R)

The reactive engine matches events to rules and gates security-relevant actions on approval.

| id | threat | mitigation in place |
|---|---|---|
| R1 | Dedup-anchor planting: an inert non-matching event becomes the dedup anchor and silently suppresses a later actionable alert sharing the key. | The dedup window anchors only on events that actually matched a rule and acted (`matched`/`gated`/`fired`), so an inert event cannot become the anchor; legitimate dedup of a genuine repeat is preserved. |
| R2 | Resolve-event forgery withdraws a pending approval and cancels its remediation job with no approval credential and no matching rule. | A resolve withdraws an approval only when scoped to the rule that raised it: same source, admitted by that rule's source filter, and matching the rule when its firing status is read. A forged resolve carrying only the dedup key matches no rule and cancels nothing; the approval then fails closed at its TTL. |
| R3 | A daily-cap flood self-disables a security-relevant rule, suppressing later real events until manual re-enable. | A daily cap throttles rather than disables: the action is withheld but the rule stays armed and keeps evaluating, every over-cap event stays visible as a suppressed row naming the rule, the cap recovers on its own when the rolling window clears, and the rule pages at max priority once per window — so a flood can neither blind the rule silently nor storm the operator. |

**Accepted residuals.**

- **R2 —** a legitimate resolve is not auto-withdrawn if the rule that raised the approval was
  deleted, disabled, or otherwise unresolvable at resolve time; the approval still fails closed
  at its TTL and the operator can reject it by hand. This is the safe direction (a resolve that
  cannot be scoped cancels nothing).
- **R3 —** the once-per-window cap page is process state, so a daemon restart re-pages a
  still-capped rule once — louder than silent, but not a swept ledger.
- **R4 —** a rule with no `--source` matches events from any hook, a cross-source spoofing
  footgun. Accepted with a documented warning; a lint/warn is future work.

## Daemon surface (D)

Endpoints and listeners the daemon exposes on the LAN/tailnet.

| id | threat | mitigation in place |
|---|---|---|
| D1 | Pre-auth argon2id memory-amplification DoS on the web `/login` and bearer endpoints. | A KDF concurrency gate (global ceiling plus per-source in-flight cap) sheds a pre-auth flood with 503 having run no argon2id; a real login still succeeds. |
| D3 | An unauthenticated `/metrics` on the state-backend listener is reachable by tofu-tier jobs. | `/metrics` is bearer-gated with an envelope-derived scrape token, compared constant-time, failing closed when no deriver is present; `doctor` presents the token. |

**Accepted residuals.**

- **D1 —** the per-source cap keys on `RemoteAddr`, so a reverse proxy (X-Forwarded-For)
  collapses sources; the global ceiling still bounds total KDF memory behind a proxy.
- **D2 —** sandbox egress is filtered by port, not destination, so a job can reach any host on
  22/443. Accepted for v1 with the posture documented; a destination allowlist is future
  hardening.
- **D4 —** the `--hook-allow` CIDR allowlist is nullified behind a reverse proxy that sets
  X-Forwarded-For. Documented limitation.

## Backup and recovery (B)

Backups must not let an attacker downgrade an authenticated target to an unauthenticated one.

| id | threat | mitigation in place |
|---|---|---|
| B1 | The git restore leg: removing `keyring.age` from the backup ref downgrades the `AUTHENTICATED.mac` forgery-refusal to a warning, so an attacker snapshot restores. | A per-target `Authenticates` fact is carried out-of-band on the recovery kit and honored at restore: a git snapshot whose in-repo tag and `AUTHENTICATED.mac` were stripped is refused as a downgrade, not trusted as a legacy target. `--allow-unauthenticated` remains the loud, explicit escape; `--target-authenticates` on `restore` and the kit's record wire it end-to-end. |

**Accepted residuals.**

- **B1 —** the kit marks every target `Authenticates`, so a genuinely legacy target needs
  `--allow-unauthenticated`. The scrape and session derive purposes are operational and not on
  the recovery card; adding them to `secrets.Purposes()` is optional if card-reproducibility is
  wanted.

## Upgrade (U)

- **U1 —** upgrade "verify" is a functional smoke test, not a staged-binary trust control:
  there is no signature or provenance check on the new binary. Accepted; closing it ties to the
  deferred cosign/SLSA supply-chain track.

## Agent surface (AI)

The three programmatic faces defined in ADR 0014: the write half of `/api/v1`, `exocomp login`
+ `--remote`, and the MCP-over-HTTP server. The threat here is a delegated agent or script —
possibly reading attacker-influenced event text — exceeding its scope or slipping a write past
the gate.

| id | threat | mitigation in place |
|---|---|---|
| AI-1 | An MCP stdio face with no credential and no scope advertised `approve_job`/`reject_job` beside `workspace_apply`, so an agent could gate its own destructive job and then approve it; `get_job_events`/`list_events` returning attacker-posted webhook text to the same session amplifies it via prompt injection. | The bespoke stdio server is removed. The one MCP surface is MCP-over-HTTP (`POST /mcp`), which authenticates with an ADR-0009 bearer token, scope-gates `tools/list` and every `tools/call`, and re-checks the token on every call so a revoke bites on the next request. A gated job is decided from the browser/phone surface, the CLI, or an `approve`-scope `/api/v1` token. |
| AI-14 | A `--require-approval` step in a composed chain was not durably gated mid-run: the Sequencer released an `awaiting_approval` step unconditionally and `ClaimJob` had no approval join, so a running daemon released and ran a gated step with no operator decision. | A gated step is marked `require_approval` in the ledger; the Sequencer arms a pending approval and parks the step in `awaiting_approval` instead of releasing it — approve releases exactly that step, reject cancels it and blocks downstream. `ClaimJob` refuses any queued job carrying a pending approval, so the gate holds at the claim however a job reached queued. |
| AI-2 | `operator` satisfying both the write and approve scopes meant the only scope that can enqueue a gated apply could also settle its approval — handing an agent the plan/apply token handed it the authority the gate withholds. | `/api/v1`'s decision route takes the `approve` scope alone, and both decision paths refuse a caller the gated job's ledger records as its actor. The browser surface keeps operator-or-approve for the human at the console. |
| AI-3 | A TOCTOU between `CreateArtifact` and the verdict event let an apply-by-id landing in the window read "no verdict" as "no policy" and enqueue plainly against a deny-all workspace. | A plan on a workspace with enabled policies and no adjudication record is unconsumable on every face; the seal records a no-match adjudication so a legitimately unmatched plan still applies; and the daemon evaluates policy before it seals, so the remaining window is one event append. |
| AI-4 | A fresh apply consulting no policy at all — omitting `plan` skipped the deny/require_approval machinery — meant `policy add --action deny` protected only the plan→apply-by-id path. | A fresh apply on a workspace with enabled plan policies is refused in the service verb, so every face inherits it; plan-then-apply-by-id is the adjudicable path. |
| AI-5 | `--assume-yes`, the only way to pin non-interactively, also overrode the changed-fingerprint refusal — so the exact invocation every script/agent must use re-pinned to whatever certificate answered and took its bearer token. | Replacing a changed fingerprint, or re-pointing an alias at another endpoint, takes the login-only `--replace-pin`; `-y` alone refuses a non-zero change and leaves `hosts.toml` untouched. |
| AI-6 | `GET /api/v1/health` let a readonly token fork eight PATH-resolved subprocesses and unwrap the keyring per request, at any rate, outside the job queue and the KDF gate. | The summary is served from a 10s cache whose refresh is serialized, so a flood of reads costs one probe. |
| AI-7 | `Idempotency-Key` as one global namespace let a planted key return another token's job for a write that never enqueued, and write the caller's attribution onto a foreign ledger. | Keys are namespaced by face, authenticated token id, and route before they reach the queue, which also puts the queue's reserved `wf:` step keys out of reach of any face. |
| AI-8 | Cookie sessions accepted on `/api/v1` write routes with no CSRF token, Origin/Referer or Content-Type check made `SameSite=Strict` the whole mitigation, and it does not scope to a port. | Write routes require an explicit bearer credential; reads still resolve a session, and the browser's own same-origin decide form is unchanged. |
| AI-9 | A saved plan applied by id with no check that it belonged to the workspace in the path ran one workspace's sealed plan in another, under that workspace's backend bearer and state passphrase. | The plan's owner is read back from its plan job's runbook and a mismatch is an `invalid_argument` before anything is enqueued or decrypted. |
| AI-10 | A revoked token kept receiving an already-open SSE job stream because auth was resolved once, at attach. | The tail re-resolves its token row on every poll (a row read, no KDF) and ends the stream when it stops authenticating, with a 12h hard lifetime as backstop. |
| AI-11 | No rate limit or lockout on bearer auth (the KDF gate caps parallelism, not rate), and the early return for an unknown/inactive row was a timing oracle for which token ids exist. | Consecutive failures back off per source and per token row (429 + `Retry-After`, no KDF spent) and are logged once per run — this is the guard that bounds guessing. The verify path looks up the row; absent, inactive, or secret-mismatch all return unauthenticated with no KDF; the constant-time digest compare on a live row is unchanged. |
| AI-12 | A `--source` playbook escaping its checkout (`../`, or an un-rebased absolute path) ran a tree the source signature never covered. | The joined path is resolved (symlinks included) and required to stay inside the checkout; an absolute playbook with `--source` is refused; a source name is charset-validated before it is spliced into any path. |
| AI-13 | MCP writes were wholly unattributed — an agent's write read off the ledger as the operator's own CLI run — and a `tools/call` sent as a notification mutated state while writing zero bytes back. | Writes record the `mcp` face, and a side-effecting method sent without an id is refused with `invalid_request` instead of executed. |

**Accepted residuals.**

- **AI-2 —** an operator driving a remote daemon needs an `approve`-scope token for
  `approval decide --remote`. The self-approval refusal keys on the token *name* in the ledger,
  so a revoked name later reused by a new token is treated as the same principal.
- **AI-3 —** the seal and the verdict are still two writes; the window is one `AppendJobEvent`
  wide and fail-closed covers it. A store-level transaction spanning `CreateArtifact` and the
  verdict event would close it structurally, and needs the store seam.
- **AI-6 —** the health probe resolves seven optional tools by bare name on the ambient PATH
  (S6) and calls `secrets.Open(dataDir)` per run to mint the `/metrics` scrape token. Deriving
  the token once at daemon start and pinning tool paths belong to `internal/health`.
- **AI-7 —** a recovered job's runbook/kind/argv are not compared against the request. With the
  namespace in place a recovered job can only be the same principal's own earlier call to the
  same verb, so the mismatch case is a retry with different arguments that still returns the
  first job.
- **AI-11 —** the timing gap between the absent/inactive and live-row auth paths is accepted,
  not closed. The equalizer that once paid a decoy argon2id to mask it never held (a live row
  hashes at `argonTime=1` while the decoy hardcoded `t=3`, a ~3× gap) and has been removed. The
  oracle leaks only which token *ids* exist — small integers already implied by the id-in-the-clear
  scheme — not secret bytes; the secret is a 128-bit token, and turning the leak into
  enumeration needs a guessing run, which the fail counter and KDF gate refuse before the KDF
  runs regardless of whether the id is live or absent. The backoff is process state and keys on
  `RemoteAddr` (a proxy collapses sources); failures are logged, not recorded on the token row,
  so a guessing run shows in the log but not in `token list`.

## Optional OpenBao Transit seal (ADR 0013)

An operator may move the DEK's wrapping key off the data volume into an OpenBao Transit key
(the auto-unseal pattern). It is **additive**: `keyring.transit` wraps the DEK via
`transit/encrypt` beside an untouched `keyring.age`, so the printed recovery card still opens
the store **without OpenBao** — recovery never depends on the vendor (invariant 7).

- **Threat addressed.** A stolen data volume with the seal enabled contains no key that
  unwraps the DEK: the host key need not be on disk, and the transit key never leaves OpenBao.
  This closes the stolen-disk gap a plaintext `host.key` leaves open, for the daemon's normal
  boot path.
- **Failure mode is explicit.** A configured-but-unreachable OpenBao fails `Open` with a clear
  error and never silently falls back to the host-key path — an outage cannot quietly downgrade
  the chosen seal. Reverting is a deliberate `exocomp seal disable`.
- **New trust.** OpenBao can read the DEK it wraps, and its own seal/audit posture joins the
  operator's threat model. The token authenticating to OpenBao is sourced from env or a file
  outside the data dir and is never written into it; neither the token nor the DEK plaintext
  reaches a log line or job event.
- **Not a defence against root** on the box, same as ADR 0003.
