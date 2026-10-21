---
type: Design
title: "Threat model"
description: "What an attacker who reaches the daemon can do, the guarding mechanisms, and the residuals that are accepted rather than closed."
status: stable
tags: [security, threat-model]
---

# Threat model

exocomp's security posture of record: the assets it protects, the trust boundaries it
draws, the adversaries and threats it considers, the mitigations in place for each, and the
residuals it knowingly accepts.

Threats are ranked by realistic risk to a **solo homelab operator** running one container on
one host, not by theoretical CVSS. The threat catalog below numbers every threat sequentially
(`T1`, `T2`, …) in document order; each entry names the mitigation in place today, pinned by a
regression test, and where a gap remains it is recorded as an accepted residual under the
threat's section.

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
  the running box is explicitly **out of scope** — the same boundary Decision 0003 draws.
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
token ids (see T27).

## Envelope

The DEK is wrapped by `host.key` and seals every per-row secret.

| id | threat | mitigation in place |
|---|---|---|
| T1 | Evil-maid / stolen data volume: a plaintext `host.key` in the data dir yields every secret with `age -d` + stdlib GCM. | `host.key` is never a bare plaintext file by default. It resolves by precedence: systemd-credential host-key wrapping (`LoadCredentialEncrypted=`, `--with-key=host`) on the Quadlet/systemd tier — the unwrap secret is `/var/lib/systemd/credential.secret`, root-only and outside the data volume, so a stolen volume lacks it; scrypt-passphrase-wrapped on the compose/non-systemd tier, the passphrase sourced from env/file outside the volume; plaintext only with a loud `doctor` warning and never written by init when a passphrase is set. TPM binding (`--with-key=host+tpm2`) is a hardware enhancement, detected and verified, never claimed when absent. `doctor` reports the real tier. |
| T2 | `identity migrate --drop-recipient` re-wrapping the *same* DEK would let a dropped recipient holding any retained `keyring.age` keep decrypting everything, including secrets sealed after the drop. | The drop rotates the DEK: every row is re-encrypted and re-wrapped only to the remaining recipients, so a dropped recipient decrypts nothing before or after the drop. |
| T3 | AEAD `name\|kind` AAD with an unescaped separator lets distinct (name, kind) rows collide, so a ciphertext opens under the wrong identity — defeating the "move a ciphertext and it won't open" property. | AAD is length-prefixed so (name, kind) encodes unambiguously; a legacy-encoding fallback on open reads rows sealed before the change. |
| T4 | The DEK and subkeys living in ordinary swappable, core-dumpable heap could leak through swap or a core dump. | The DEK is `mlock`'d with a managed lifecycle; `doctor` reports its real state. |
| T5 | The T2 rotation re-encrypting rows one at a time and flipping `keyring.age` only at the end leaves a crash mid-loop with some rows under the new DEK and the rest under the old, while `keyring.age` still wraps the old — silent data loss on the exact path revocation serves. | `RotateDEK` stages the new keyring and metadata (`keyring.age.next`, `recipients.next`/`keyring.version.next`, and `host.key.next` on a host rotation) *before* any row moves, re-encrypts every row under the new DEK, then promotes atomically with `keyring.age` renamed **last** — the single commit point. `secrets.Reconcile` detects a leftover `keyring.age.next` on the next `Open`, loads both DEKs, re-seals every row under the new one (decrypting each under whichever DEK holds it), and promotes — idempotent, so a crash during reconciliation converges on the next call. `RotateDEK` refuses to stack a fresh rotation on a pending one. All T1–T4 properties are preserved across the rotation. |

**Accepted residuals.**

- **Host-key wrapping —** the systemd-credential tier is unit-tested on the read side; the live
  `systemd-creds` / rootless-podman path still needs validation on a real host.
- **Rotation sequencing —** rotation is CLI-only and daemon-stopped, not a sequenced job with
  rollback/approval, and derived consumers (restic repos, tofu state) are not auto-re-keyed
  (they are printed as manual follow-through). A daemon-sequenced rotation is future work.
- **Legacy AAD migration —** rows sealed under the legacy AAD keep the fallback on open, so the
  collision persists for un-migrated rows until a one-time re-seal migration runs.
- **Keyring version-file trust —** the keyring-generation downgrade refusal trusts an
  unauthenticated root version file and no-ops on version 0. Accepted: the DEK now rotates
  (T2), which bounds the blast radius. Authenticating the version file is future hardening.
- **Transit-sealed reconciliation —** a transit-sealed store (auto-unseal, no host key on disk)
  whose reseal is interrupted cannot reconcile without the host key, because reconciliation
  resolves the DEKs through the age host-key path. The host-key path is the supported recovery;
  a transit-native reconcile is future work.

## Sources and supply chain

Playbooks, tofu, and their dependencies are untrusted-source-derived until proven otherwise.

| id | threat | mitigation in place |
|---|---|---|
| T6 | Dependency resolution (`uv pip install`) running in the daemon context, before the job sandbox, lets a poisoned `pyproject.toml`/`requirements.txt` execute with daemon privileges. | The resolver runs under the job sandbox tier: every `uv` invocation is wrapped in the nono launcher, granted its venv cache, the manifest, and HTTPS, and denied the data dir and keys by the profile guard. A manifest whose install step reads a denied path fails closed. |
| T7 | A `--signed` ref gate falling back to ambient host git config when `allowed_signers` is empty accepts an attacker-signed ref. | `--signed` fails closed with no exocomp-owned allowed-signers and never consults ambient git config; `source add --signed` refuses at add time without `--allowed-signers`/`--signer`; verification is pinned to ssh signature format. |
| T8 | A pyproject dependency specifier passed as raw argv to `uv` allows flag injection (`--index-url=…`, `-e /etc`). | A specifier beginning with `-` is refused at parse; accepted specifiers reach `uv` only after `--`. |
| T9 | Git source-sync using trust-on-first-use for the forge host key (the `known_hosts` pin of Decision 0010 never applied) allows a first-sync MITM. | An `ssh://` source pins the forge host key from the recorded `known_host` store: strict verification when the key is recorded, a host-key mismatch fails the sync. |

**Accepted residuals.**

- **Resolver sandbox validation —** the resolver profile's grant set is validated against the
  sandbox by unit test with a stand-in `uv`; a live `uv` resolve under the sandbox on a real
  host — fetching a real index, seeding a real interpreter — still needs on-host validation.
- **Resolver reads source-named local artifacts —** an ansible requirements file may name a
  local collection path (`type: dir`/`type: file`) outside the project root, and the resolver
  sandbox grants exactly those read-only so the confined `ansible-galaxy` can read their
  `galaxy.yml`. A poisoned requirements file can therefore point the resolver at any existing
  host path that does not reach the data dir. Accepted because the source is operator-added,
  the grant is read-only, and the profile guard still denies the data dir and keys; scoping
  local artifacts to the source tree is future hardening.
- **Shared uv cache —** every source's resolver is granted write on one uv cache,
  `cache/python/uv-cache`, and each venv is hardlink-seeded from it (Decision 0011). A
  poisoned manifest's build backend can therefore change cache entries that a later resolve
  of another source installs from, and the files already hardlinked into other sources'
  venvs, since hardlinks share inodes. Accepted because sources are operator-added and the
  grant never reaches `exocomp.db` or the keys. A cache per source, at the cost of
  downloading shared packages again, is the hardening.
- **Unsigned path sources —** unsigned is the default; path sources have no signature gate.
  Accepted for now with the trust posture documented; a signed-source opt-in for local trees is
  future hardening.
- **Forge host-key TOFU —** the pin consumes the `known_host` store but does not itself record
  the forge key (recording is `host onboard` / `ssh-keyscan`), so a forge's first sync is TOFU
  until its key is recorded; a non-standard forge SSH port needs its `known_host` row keyed as
  `[host]:port`.
- **Tool-path pinning —** pinned tools are resolved by bare name on the ambient PATH, with no
  absolute pin or checksum. Future hardening.
- **Local-exec to the `exocomp` CLI —** a playbook (`command`/`shell`/`local_action`) or a tofu
  `local-exec` step can invoke the local `exocomp` binary, which mints tokens and unwraps secrets
  by opening the data dir directly, bypassing every HTTP guard. Under the sandboxed tier this fails
  closed by the same guard as T6: the profile never grants the data dir and name-denies
  `exocomp.db`, `host.key`, and `keyring.age`, so an invoked CLI cannot open the store or the
  envelope — no token mints, no secret unwraps. **Residual:** under `TierDisabled` (explicit
  `--sandbox disabled`, or the `opportunistic` tier degraded on a host without Landlock) there is
  no kernel policy, so a local-exec step runs with the daemon's own filesystem access and *can*
  mint an operator token or read `host.key`. `TierDisabled` is therefore a trusted-playbook tier;
  the `opportunistic`→`disabled` degradation is loud in the daemon log and `doctor` but invisible
  to a playbook author, and must be treated as a security-relevant downgrade. Untrusted playbooks
  require the sandboxed tier.
- **Per-run privileged (unsandboxed) escape hatch —** a single run can opt out of the kernel
  sandbox for one job (`--privileged` on the run verbs; a "Run unsandboxed (privileged)" checkbox
  on the source launcher), which runs that job at `TierDisabled` even when the daemon default is
  `enforcing`. This is the same trusted-playbook posture as a `disabled`-tier daemon, scoped to one
  job: the privileged job *can* reach the data dir, `host.key`, `keyring.age`, and the database, and
  can mint tokens via the local CLI. It exists for trusted, operator-authored work that legitimately
  needs host access, and is gated accordingly — **secure by default** (absent the explicit request
  nothing changes; the default stays `enforcing`), **operator-scoped only** (the web and API write
  faces refuse a readonly or approve token by the same operator-scope guard every run passes; the
  local CLI operator is already trusted), **explicit and loud** (the CLI prints an unsandboxed
  warning; the web control is a deliberately-warned, non-default checkbox), and **attributed +
  audited** (the marker is persisted on the job row, an attributed ledger event records who
  requested it, and the queue records the unsandboxed execution on every path). It never weakens the
  guard for a non-privileged run. A sandbox denial's own hint names the sandbox-preserving answers
  first — an ssh identity grant (`--ssh-identity`), or reaching a self-host target over SSH
  (ssh-to-self) — and names `--privileged` only as the last resort, so the escape hatch is not the
  path of least resistance. An untrusted playbook must not be run privileged.

## Reactive engine and approvals

The reactive engine matches events to rules and gates security-relevant actions on approval.

| id | threat | mitigation in place |
|---|---|---|
| T10 | Dedup-anchor planting: an inert non-matching event becomes the dedup anchor and silently suppresses a later actionable alert sharing the key. | The dedup window anchors only on events that actually matched a rule and acted (`matched`/`gated`/`fired`), so an inert event cannot become the anchor; legitimate dedup of a genuine repeat is preserved. |
| T11 | Resolve-event forgery withdraws a pending approval and cancels its remediation job with no approval credential and no matching rule. | A resolve withdraws an approval only when scoped to the rule that raised it: same source, admitted by that rule's source filter, and matching the rule when its firing status is read. A forged resolve carrying only the dedup key matches no rule and cancels nothing; the approval then fails closed at its TTL. |
| T12 | A daily-cap flood self-disables a security-relevant rule, suppressing later real events until manual re-enable. | A daily cap throttles rather than disables: the action is withheld but the rule stays armed and keeps evaluating, every over-cap event stays visible as a suppressed row naming the rule, the cap recovers on its own when the rolling window clears, and the rule pages at max priority once per window — so a flood can neither blind the rule silently nor storm the operator. |

**Accepted residuals.**

- **Unscoped resolve handling —** a legitimate resolve is not auto-withdrawn if the rule that
  raised the approval was deleted, disabled, or otherwise unresolvable at resolve time; the
  approval still fails closed at its TTL and the operator can reject it by hand. This is the
  safe direction (a resolve that cannot be scoped cancels nothing).
- **Cap-page restart behavior —** the once-per-window cap page is process state, so a daemon
  restart re-pages a still-capped rule once — louder than silent, but not a swept ledger.
- **Sourceless rule matching —** a rule with no `--source` matches events from any hook, a
  cross-source spoofing footgun. Accepted with a documented warning; a lint/warn is future
  work.

### Checks, netgroups, and peer stamping

The check sweep turns probe transitions into events in the same pipeline; netgroups give
the operator a name for an address class; the ingress stamps a webhook's peer. New rows are
numbered T30+ to avoid renumbering the existing catalog.

| id | threat | mitigation in place |
|---|---|---|
| T30 | A spoofed home-presence claim (a webhook body asserting a host is home, or a forged peer) fires a run from outside the house. | The peer verdict is stamped by the ingress from the physical connection (`peer_netgroup`), never from a client claim; a check's location is the address the daemon itself probed. A claim never crosses into the verdict. |
| T31 | A check flood re-arms a rule past its budget (an interval set too short, or a target flapping up/down). | Transition-only emission with N-consecutive-same-result hysteresis (default 3); per-target dedup and per-target cooldown for the check source; the daily cap throttles rather than disables. |
| T32 | DHCP re-leases a target's home address to a different device, so a check reports the wrong host as present. | Stated precondition ([design/check-triggered-runs.md](../design/check-triggered-runs.md) §3.2.1): targets are pinned by static reservation or fixed lease. The netgroup classifies the address, never the host. |
| T33 | An overlay subnet route (tailscale advertising the home LAN) keeps a checked address reachable from anywhere while an overlay host answers at that address. | An overlay route still requires a live host at the probed address (real presence); the residual is address-uniqueness (T32). LAN-only routing is a documented trust boundary — the sweep has no routing-table authority from inside the container. |
| T34 | A daemon restart reads every up target as a fresh down→up transition and fires every presence rule. | Cold-start seeding writes `check_state` with `emitted = result` and emits nothing on first observation; only a change from a previously-emitted status is a transition. A target home at restart is missed once — fail-safe. |
| T35 | An http check persists a health endpoint's body (a secret, a stack trace, attacker text). | The http probe reads the status line only and closes the body without reading it (AGENTS.md 4); no body ever reaches the event or a ledger. |
| T36 | An operator-created check targets the daemon's own loopback listeners or the cloud metadata service, making the check's result depend on (or probe) a service the operator did not mean to reach. | Every dialing kind applies the same address-class refusal whenever `AllowLoopback` is false (the check path; a gate sets it true): tcp dials through a `net.Dialer.Control` hook, tls handshakes through one too (its default dialer guards the connected IP, so a hostname is covered), http/metric keep the guarded client, and ping refuses a refused literal before the subprocess runs. The guard refuses loopback, link-local unicast (covers `169.254.169.254`), unspecified, and multicast at the connected IP — the same dial-time check T16 uses, scoped so RFC1918/ULA LAN targets (the feature's purpose) are still allowed. A check against a refused address observes down. A tls address must name a host: `:443` is refused at validation rather than dialing the local host. |

**Accepted residuals.**

- **Overlay-routing precondition is documentary, not enforced —** the LAN-only-routing
  requirement (T33) cannot be asserted from inside the container at dial time; it is a
  stated deployment posture. A host configured to route the home LAN over an overlay
  silently weakens the location guarantee.
- **DNS-rebinding on name targets —** tcp/ping targets are IP literals by construction; an
  http/metric URL or a tls hostname resolves at dial time through the guard's `Control` hook,
  which re-checks the connected IP, so a re-pointed name is refused. The name is otherwise
  trusted, and the check carries no credential, so a hostile-but-public target yields a
  down/up boolean only.

## Daemon surface

Endpoints and listeners the daemon exposes on the LAN/tailnet.

| id | threat | mitigation in place |
|---|---|---|
| T13 | Pre-auth argon2id memory-amplification DoS on the web `/login` and bearer endpoints. | A KDF concurrency gate (global ceiling plus per-source in-flight cap) sheds a pre-auth flood with 503 having run no argon2id; a real login still succeeds. |
| T14 | An unauthenticated `/metrics` on the state-backend listener is reachable by tofu-tier jobs. | `/metrics` is bearer-gated with an envelope-derived scrape token, compared constant-time, failing closed when no deriver is present; `doctor` presents the token. |

**Accepted residuals.**

- **Proxy source collapsing —** the per-source cap keys on `RemoteAddr`, so a reverse proxy
  (X-Forwarded-For) collapses sources; the global ceiling still bounds total KDF memory behind
  a proxy.
- **Port-only egress filtering —** sandbox egress is filtered by port, not destination, so a
  job can reach any host on 22/443. Accepted for v1 with the posture documented; a destination
  allowlist is future hardening. A per-source default egress set ([Decision 0023](../decisions/0023-source-egress-defaults.md))
  widens an ansible run the same way and persists the grant on the source. It is operator-set in
  the store and never read from repo content, so a repository cannot widen its own reach, but it
  can name the daemon's own listener or state-backend ports; that reach stays closed on the
  credential side, per the paragraph below. The one combination this would otherwise open — a job reaching
  the API port and holding a token to drive it — is closed on the credential side: a job
  receives no access token by default, and the run-secret resolver refuses to inject a sealed
  value shaped like one (`exo_<id>_<secret>`), so an operator cannot bind the daemon's own
  credential into a run's env and turn the job into a deputy for the API. Token minting over HTTP
  exists only on the browser surface (`POST /tokens/new`), gated by an operator cookie session
  **and** same-origin (`Sec-Fetch-Site`) **and** a proof-of-possession re-auth — none of which a
  non-browser job can present; `/api/v1` and `/mcp` expose no mint at all. (The local `exocomp`
  binary does mint, but a job reaching it is the local-exec vector above, not an HTTP path.)
- **Hook allowlist behind proxy —** the `--hook-allow` CIDR allowlist is nullified behind a
  reverse proxy that sets X-Forwarded-For. Documented limitation.

## Outbound push endpoints (SSRF)

A device registers the URL of its UnifiedPush distributor over `POST /api/v1/notify/endpoints`
and the daemon POSTs each approval/job notification to it. The destination is **client-supplied**
and the daemon is the one making the request, so a registered endpoint is a server-side request
forgery vector: pointed at an internal address it would turn the daemon into a proxy for the LAN,
the loopback listeners (the daemon's own API, `/metrics`), or the cloud instance-metadata service.

| id | threat | mitigation in place |
|---|---|---|
| T16 | A registered push endpoint names an internal address (loopback, RFC1918/ULA, link-local, `169.254.169.254`), so the daemon's own POST reaches a service the registrant cannot reach directly — SSRF, including via DNS rebinding after registration. | Two independent layers. **Registration:** the URL must be `https`, carry no credentials, and its host must resolve *exclusively* to public addresses; a literal private IP or a hostname that resolves to loopback/private/link-local/ULA/metadata/multicast/unspecified/broadcast is refused `400` before any row is stored. **Send time (load-bearing):** the transport's HTTP client dials through a `net.Dialer.Control` hook that re-checks the **actual connected IP** against the same deny-list and refuses the socket — so a name re-pointed at a private address *after* registration (rebinding) is still blocked, because the check is on the address dialed, not the name stored. Per-token endpoint caps bound outbound fan-out; a `404`/`410` self-prunes the endpoint; per-endpoint failures are isolated. |

Why a registered endpoint cannot make the daemon reach internal services: the registration check
stops the obvious literals and the current resolution, and the dial-time check stops everything the
registration check cannot — a TTL-0 record that resolves public once (to pass registration) and
private thereafter never gets a socket, because `Control` runs on the concrete post-resolution IP of
every connection attempt, registration-time or not.

**Accepted residuals.**

- **Public-but-sensitive destinations —** the deny-list is address-class based (private/loopback/
  link-local/metadata), not a destination allowlist, so a registered endpoint may still point at any
  *public* host. The body carried is the same values-free notification the other transports send (a
  title, a deep link, non-sensitive tags — never a plan, a secret, or a decision token), so the blast
  radius of a hostile-but-public endpoint is a spurious push, not a disclosure.

## Backup and recovery

Backups must not let an attacker downgrade an authenticated target to an unauthenticated one.

| id | threat | mitigation in place |
|---|---|---|
| T15 | The git restore leg: removing `keyring.age` from the backup ref downgrades the `AUTHENTICATED.mac` forgery-refusal to a warning, so an attacker snapshot restores. | A per-target `Authenticates` fact is carried out-of-band on the recovery kit and honored at restore: a git snapshot whose in-repo tag and `AUTHENTICATED.mac` were stripped is refused as a downgrade, not trusted as a legacy target. `--allow-unauthenticated` remains the loud, explicit escape; `--target-authenticates` on `restore` and the kit's record wire it end-to-end. |

**Accepted residuals.**

- **Legacy target marking —** the kit marks every target `Authenticates`, so a genuinely
  legacy target needs `--allow-unauthenticated`. The scrape and session derive purposes are
  operational and not on the recovery card; adding them to `secrets.Purposes()` is optional if
  card-reproducibility is wanted.

## Upgrade

- **Binary provenance check —** upgrade "verify" is a functional smoke test, not a
  staged-binary trust control: there is no signature or provenance check on the new binary.
  Accepted; closing it ties to the deferred cosign/SLSA supply-chain track.

## Agent surface

The three programmatic faces defined in Decision 0014: the write half of `/api/v1`, `exocomp login`
+ `--remote`, and the MCP-over-HTTP server. The threat here is a delegated agent or script —
possibly reading attacker-influenced event text — exceeding its scope or slipping a write past
the gate.

| id | threat | mitigation in place |
|---|---|---|
| T16 | An MCP stdio face with no credential and no scope advertised `approve_job`/`reject_job` beside `workspace_apply`, so an agent could gate its own destructive job and then approve it; `get_job_events`/`list_events` returning attacker-posted webhook text to the same session amplifies it via prompt injection. | The bespoke stdio server is removed. The one MCP surface is MCP-over-HTTP (`POST /mcp`), which authenticates with an Decision 0009 bearer token, scope-gates `tools/list` and every `tools/call`, and re-checks the token on every call so a revoke bites on the next request. A gated job is decided from the browser/phone surface, the CLI, or an `approve`-scope `/api/v1` token. |
| T17 | A `--require-approval` step in a composed chain was not durably gated mid-run: the Sequencer released an `awaiting_approval` step unconditionally and `ClaimJob` had no approval join, so a running daemon released and ran a gated step with no operator decision. | A gated step is marked `require_approval` in the ledger; the Sequencer arms a pending approval and parks the step in `awaiting_approval` instead of releasing it — approve releases exactly that step, reject cancels it and blocks downstream. `ClaimJob` refuses any queued job carrying a pending approval, so the gate holds at the claim however a job reached queued. |
| T18 | `operator` satisfying both the write and approve scopes meant the only scope that can enqueue a gated apply could also settle its approval — handing an agent the plan/apply token handed it the authority the gate withholds. | `/api/v1`'s decision route takes the `approve` scope alone, and both decision paths refuse a caller the gated job's ledger records as its actor. The browser surface keeps operator-or-approve for the human at the console. |
| T19 | A TOCTOU between `CreateArtifact` and the verdict event let an apply-by-id landing in the window read "no verdict" as "no policy" and enqueue plainly against a deny-all workspace. | A plan on a workspace with enabled policies and no adjudication record is unconsumable on every face; the seal records a no-match adjudication so a legitimately unmatched plan still applies; and the daemon evaluates policy before it seals, so the remaining window is one event append. |
| T20 | A fresh apply consulting no policy at all — omitting `plan` skipped the deny/require_approval machinery — meant `policy add --action deny` protected only the plan→apply-by-id path. | A fresh apply on a workspace with enabled plan policies is refused in the service verb, so every face inherits it; plan-then-apply-by-id is the adjudicable path. |
| T21 | `--assume-yes`, the only way to pin non-interactively, also overrode the changed-fingerprint refusal — so the exact invocation every script/agent must use re-pinned to whatever certificate answered and took its bearer token. | Replacing a changed fingerprint, or re-pointing an alias at another endpoint, takes the login-only `--replace-pin`; `-y` alone refuses a non-zero change and leaves `hosts.toml` untouched. |
| T22 | `GET /api/v1/health` let a readonly token fork eight PATH-resolved subprocesses and unwrap the keyring per request, at any rate, outside the job queue and the KDF gate. | The summary is served from a 10s cache whose refresh is serialized, so a flood of reads costs one probe. |
| T23 | `Idempotency-Key` as one global namespace let a planted key return another token's job for a write that never enqueued, and write the caller's attribution onto a foreign ledger. | Keys are namespaced by face, authenticated token id, and route before they reach the queue, which also puts the queue's reserved `wf:` step keys out of reach of any face. |
| T24 | Cookie sessions accepted on `/api/v1` write routes with no CSRF token, Origin/Referer or Content-Type check made `SameSite=Strict` the whole mitigation, and it does not scope to a port. | Write routes require an explicit bearer credential; reads still resolve a session, and the browser's own same-origin decide form is unchanged. |
| T25 | A saved plan applied by id with no check that it belonged to the workspace in the path ran one workspace's sealed plan in another, under that workspace's backend bearer and state passphrase. | The plan's owner is read back from its plan job's runbook and a mismatch is an `invalid_argument` before anything is enqueued or decrypted. |
| T26 | A revoked token kept receiving an already-open SSE job stream because auth was resolved once, at attach. | The tail re-resolves its token row on every poll (a row read, no KDF) and ends the stream when it stops authenticating; the credential epoch is pinned at attach, so a rotated token — whose row stays Active — ends the stream too, with a 12h hard lifetime as backstop. Both stream faces (approvals and job tails) are additionally capped per process and per identity (8 total, 4 per token) in a shared funnel, so a fleet of stale tabs cannot multiply the one-connection store pool's load without bound; an over-cap attach is refused with 503 + `streams_over_cap`. |
| T27 | No rate limit or lockout on bearer auth (the KDF gate caps parallelism, not rate), and the early return for an unknown/inactive row was a timing oracle for which token ids exist. | Consecutive failures back off per source and per token row (429 + `Retry-After`, no KDF spent) and are logged once per run — this is the guard that bounds guessing. The verify path looks up the row; absent, inactive, or secret-mismatch all return unauthenticated with no KDF; the constant-time digest compare on a live row is unchanged. |
| T28 | A `--source` playbook escaping its checkout (`../`, or an un-rebased absolute path) ran a tree the source signature never covered. | The joined path is resolved (symlinks included) and required to stay inside the checkout; an absolute playbook with `--source` is refused; a source name is charset-validated before it is spliced into any path. |
| T29 | MCP writes were wholly unattributed — an agent's write read off the ledger as the operator's own CLI run — and a `tools/call` sent as a notification mutated state while writing zero bytes back. | Writes record the `mcp` face, and a side-effecting method sent without an id is refused with `invalid_request` instead of executed. |
| T37 | The TUI's write keys (pause/mute/approve/reject/cancel/toggle) run with operator authority in-process — and `ssh-serve` exposes the same surface to every `authorized_keys` holder — so a delegated key is an operator by construction, with no Decision 0009 scope like the web's `writeGuard`. | The TUI surface is deliberately operator-authority at the data dir, matching the CLI's decide seam (documented in DESIGN.md): approve/reject still go through the reactive engine's token, TTL and stale-plan checks and the enqueue-only queue never drains (invariant 5), so the queue's own guardrails hold regardless of the caller's nominal scope. `ssh-serve` authentication is public-key only via `authorized_keys`; every accepted session is an operator by that credential. |

**Accepted residuals.**

- **Approval token-name reuse —** an operator driving a remote daemon needs an `approve`-scope
  token for `approval decide --remote`. The self-approval refusal keys on the token *name* in
  the ledger, so a revoked name later reused by a new token is treated as the same principal.
- **Seal/verdict write window —** the seal and the verdict are still two writes; the window is
  one `AppendJobEvent` wide and fail-closed covers it. A store-level transaction spanning
  `CreateArtifact` and the verdict event would close it structurally, and needs the store seam.
- **Health-probe token minting —** the health probe resolves seven optional tools by bare name
  on the ambient PATH (see the sources tool-pinning residual) and calls `secrets.Open(dataDir)`
  per run to mint the `/metrics` scrape token. Deriving the token once at daemon start and
  pinning tool paths belong to `internal/health`.
- **Idempotency replay mismatch —** a recovered job's runbook/kind/argv are not compared
  against the request. With the namespace in place a recovered job can only be the same
  principal's own earlier call to the same verb, so the mismatch case is a retry with different
  arguments that still returns the first job.
- **Auth-timing oracle —** the timing gap between the absent/inactive and live-row auth paths
  is accepted, not closed. The equalizer that once paid a decoy argon2id to mask it never held
  (a live row hashes at `argonTime=1` while the decoy hardcoded `t=3`, a ~3× gap) and has been
  removed. The oracle leaks only which token *ids* exist — small integers already implied by
  the id-in-the-clear scheme — not secret bytes; the secret is a 128-bit token, and turning the
  leak into enumeration needs a guessing run, which the fail counter and KDF gate refuse before
  the KDF runs regardless of whether the id is live or absent. The backoff is process state and
  keys on `RemoteAddr` (a proxy collapses sources); failures are logged, not recorded on the
  token row, so a guessing run shows in the log but not in `token list`.
- **Config writes leave no actor —** the check_*/netgroup_* writes on the MCP and `/api/v1`
  faces call the store verbs directly and record neither an actor nor a ledger row
  (`internal/mcp/writes.go`, `internal/web/api_write.go`, `internal/store/check.go`), exactly
  as the existing silences and mutes already write. An agent's config change is therefore not
  attributable after the fact. Closing it needs a store-level writer/ledger seam; it is not
  attempted here.

## Optional OpenBao Transit seal (Decision 0013)

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
- **Not a defence against root** on the box, same as Decision 0003.
