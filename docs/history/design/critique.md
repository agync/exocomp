# Design critique — completeness review

Scope: `docs/design/candidate-compose-first.md`, `candidate-thin-core.md`, `candidate-hybrid.md`
reviewed against the project goals and `docs/research/*.md`. Date: 2026-08-21.

Shorthand used below: **CF** = compose-first, **TC** = thin-core, **HY** = hybrid.

## Critic gaps

### A. Disaster recovery — the chain does not actually close

**A1. The recovery kit omits the credentials needed to *reach* the backup. (all three, critical)**

Every candidate builds the same chain: printed age identity → derive/unwrap → restic password →
restore. But restic cannot open an S3/B2 repository without an access key ID and secret, and none
of the three kits carry them:

- CF §4.2 kit = age paper identity, restic repo password, tofu passphrase, repo URLs.
- TC §6.4 kit item 2 = "backup target URIs (repo URLs only — **no credentials**; those are in the
  keyring)" — and the keyring is `keyring.age`, fetched from the bucket you cannot yet read.
- HY §4.3 kit = "WHAT THIS DOES NOT CONTAIN: Your S3/GitHub credentials (**they are IN the
  backup**, encrypted to this key)."

That is a literal circular dependency: the credential required to fetch the backup lives inside the
backup. TC hand-waves it as "unauthenticated read or S3 creds from kit" — the first is a bad idea
(the keyring is safe to publish, the restic repo's existence and size are not), the second
contradicts its own kit spec. HY's DR table (step 2) even routes around it by suggesting the NAS
leg, which does not help an operator whose house burned down.

*Fix:* put the read-capable object-storage credentials (or a dedicated read-only key) on the printed
kit, and make the DR drill run from a machine that has *only* the kit — no host state, no shell
history, no password manager.

**A2. HY does not back up the ops repo at all.** §5.1: "git clones of the ops repo — **not backed
up** — remote URL + branch recorded in the manifest instead", and §5.3 step 7 re-clones from those
URLs. If the git remote is the thing that died (GitHub outage, account lock, self-hosted Forgejo on
the same dead host — a common homelab layout), restore stops. CF and TC both `git bundle` the repo
into the snapshot; HY should too. This is the difference between three backup legs and three legs
plus one unreplicated single point of failure.

**A3. The printed manual-recovery recipe is not executable as written. (TC, high)**
TC §8 step 2 is the design's headline safety property ("recovers everything with `age` + `openssl` +
`restic` and no project code at all"). The command as printed —
`openssl kdf -keylen 32 -kdfopt digest:SHA256 -kdfopt hexkey:$(xxd -p -c256) -kdfopt info:… -kdfopt salt:… HKDF`
— has `xxd` reading nothing, feeds a pipeline `openssl kdf` does not consume from stdin, and mixes
`-kdfopt` spellings. A DR card whose commands do not run is worse than no card. Any candidate that
ships a manual path must execute that exact text in CI on a clean image and diff the output against
the daemon's own derivation.

**A4. No design states what happens to in-flight jobs across a crash/restart or a restore.**
A container restart kills `ansible-playbook` mid-run; the job row stays `running` forever, and a
restored snapshot always contains at least one such row (in CF, *guaranteed*, because the backup is
itself a Semaphore task — see B2). Boot-time reconciliation ("any job in `running`/`queued` whose
PID is gone → `unknown`, notify, never auto-retry") is unspecified in all three, and the tofu
lock table makes it worse: a killed apply leaves `tflock` held with no owner. TC/HY have
`force-unlock`; neither describes automatic stale-lock detection on boot.

### B. Backup mechanics that will not hold

**B1. The GitHub leg's retention does not reclaim anything. (all three, high)**
CF ("pruned on a retention schedule"), TC ("prune >N"), HY ("prune beyond retention, default 30")
all assume `git rm` + commit + push shrinks the repo. It does not — every `.age` blob ever pushed
stays reachable from history forever. At HY's own stated tens-of-MB-per-snapshot with a nightly
schedule, the repo crosses GitHub's 5 GB guidance in well under a year and hits push rejections.
The only workable shapes are (a) a single orphan commit force-pushed each run, (b) a rolling
`git checkout --orphan` + `push --force` retention window, or (c) GitHub Releases/LFS. Also
unaddressed: GitHub's 100 MB per-file hard limit (HY at least has a 500 MB staging guard, but the
guard is on the wrong axis), and whether this use is within GitHub's acceptable-use terms.

**B2. CF's backup can never capture a quiescent database.** CF §5.1 makes the backup a Semaphore
template on a Semaphore schedule — which means `semaphore.db` always has a live, running task row
(the backup itself) at snapshot time, and any restore resurrects a job stuck in `running`. HY is the
only candidate with an explicit quiesce/drain window (§5.2, pause scheduling → drain ≤120 s → flag
`hot` if drain times out). CF should either quiesce out-of-band or accept and document that its
job history is always restored dirty.

**B3. `VACUUM INTO` is asserted against a database that may not be SQLite.** CF §5.2 runs
`sqlite3 VACUUM INTO` on `semaphore.db`; HY asserts BoltDB (`semaphore.bolt`) and uses a "quiesced
file copy + sha256". The research contradicts itself here (`ansible-executors.md` says
BoltDB/Postgres/MySQL; `prior-art.md` and `bootstrap-recovery.md` say SQLite/MySQL/Postgres). CF is
the only one that flags this as a risk (R2) — HY builds a filename, a Litestream exclusion, and a
restore fallback on the unverified answer. Verify before either design is costed; if Bolt,
`bbolt`'s `Tx.WriteTo`/`bbolt copy` is the correct consistent-snapshot primitive, not `cp`.

**B4. CF streams unencrypted Litestream WAL to S3.** §2.4/§5.2 send `semaphore.db` and `ctl.db` WAL
frames to "the same S3 bucket". Litestream does not encrypt (HY §11 honours this and disables it by
default; TC calls it optional). Those DBs hold job output — which routinely contains hostnames,
inventory, and, absent a redaction filter (CF has none), secret values. CF's own §5.2 diagram shows
the Litestream leg terminating at the same third-party S3 endpoint as the restic leg, defeating the
"restic encrypts before anything leaves the host" property in the paragraph above it.

**B5. Nobody checks that the NAS is actually mounted.** All three treat "SMB/NFS mount = local path,
zero NAS-specific code" as a feature. The corresponding failure is that a stale/unmounted
`/mnt/nas/control` becomes a *local directory* and restic happily fills the control host's own disk
with a second copy of the backup — silently, with a green job. A mountpoint + free-space + canary
file assertion before every NAS-leg run is ~10 lines and is missing from all three. Related: SMB
credentials live in host `/etc/fstab`, outside everything the design backs up or manages, and no
candidate mentions them.

**B6. No disk-full or retention policy for job logs in CF/HY.** TC prunes `job_event` at 90 days
and gzips stdout to `artifact`. CF and HY store all task output in Semaphore's DB with no stated
retention, on the same volume as the SQLite/Bolt files and the tofu state blobs. A full volume
corrupts the database *and* breaks the backup that would have saved you. Needs a retention policy,
a free-space guard that refuses new jobs, and an alert well before the cliff.

### C. Guardrails that can be bypassed

**C1. In CF and HY, Semaphore's own scheduler and Run button bypass every guardrail. (critical)**
Both designs put approvals, cooldowns, daily caps, `destructive ⇒ approval`, and the global apply
concurrency lock in the companion binary (`ctl`/`keel`) — but leave *schedules* and the *web Run
button* with Semaphore (HY §1 table: "Cron schedules, job history, run RBAC → Semaphore"; CF §3.2
defines `schedules:` as Semaphore cron entries). So a scheduled `tofu-apply`, or one click in the
proxied Vue UI, dispatches a task that never passes through the gate. CF §3.2 even writes
`tofu-apply: { concurrency: 1, approval: required }` into `control.yaml` as if Semaphore enforced
those; it does not — Semaphore has no approval concept, and per-template serialization is an
unverified assumption about the OSS build. Either the companion must be the *only* dispatch path
(disable Semaphore's scheduler, block its task endpoint, hide the Run button), or the guardrails are
advisory. TC does not have this problem because it owns the queue.

**C2. `keel key rotate-ek` silently destroys Semaphore's key store. (HY, high)** §4.1 derives
`SEMAPHORE_ACCESS_KEY_ENCRYPTION` from the EK. §4.4's rotation re-encrypts `secret.ciphertext`,
re-keys restic, and re-encrypts tofu state — but never re-encrypts Semaphore's key store, whose
every SSH key, git credential and ansible-vault password is encrypted under the old derived value.
Post-rotation, Semaphore cannot decrypt any of them and there is no stated recovery. Rotation must
either export/re-import the key store around the change, or `SEMAPHORE_ACCESS_KEY_ENCRYPTION` must
be a *stored* secret (rotated on Semaphore's own schedule) rather than a derived one.

**C3. No secret redaction in CF or HY.** TC §6.2 has a redaction filter over `job_event` plus a
belt-and-braces scan against the known-secret set, and fuzzes it (§13.2). CF and HY rely on
Semaphore's log capture, which does not redact. `ansible-playbook -vvv`, a `tofu plan` over a
non-`sensitive` variable, or a role that forgets `no_log:` writes plaintext secrets into job history
→ into the nightly snapshot → onto the NAS, S3, *and* the GitHub repo. This is the most likely way
any of these designs leaks a secret in practice.

**C4. Fleet-wide `ansible-pull` has no gate and no signature check.** CF §3.4 (and HY as an option)
puts `ansible-pull` on a systemd timer on every managed host, against the same ops repo the control
plane can write to. A bad or hostile commit reaches the whole fleet with no approval, no cooldown,
no dry-run, and no rollback — the exact class of event every candidate's §6 guardrails exist to
prevent, arriving through a door those guardrails do not cover. TC is the only design with
`repo.verify_signatures` in its schema, and even it never says where the trusted signing keys come
from or how they survive DR.

### D. Named requirements no candidate covers

**D1. PagerDuty-style escalation is a stated project goal and is deferred by all three.**
CF §6.3 ("explicitly out of scope for v1", GoAlert named as fallback), TC §12 line 11 (half a week
for "notifications, healthchecks pings, escalation", unspecified), HY §6.3 (the entire feature is
"re-publish at priority=urgent at 50% of TTL"). Nobody offers acknowledgement tracking, an
escalation chain, an on-call rotation, or "keep trying until acked" — and nobody says what escalates
when the control plane *itself* is the thing that is down. The dead-man's-switch answer
(healthchecks.io) is an unmanaged external dependency in all three: never backed up, never
provisioned by the bootstrap wizard, and its own ping URL is a credential that is not in the
recovery kit. Either scope the goal down explicitly ("v1 = ntfy priority levels; escalation is
GoAlert, post-v1") or design the seam concretely.

**D2. ansible-vault integration is asserted, not designed.** All three say "keep it as a secondary
path" and stop. Undesigned: multiple `--vault-id` labels per project (real repos have `dev`/`prod`
vault ids), rotation across an existing repo (`ansible-vault rekey` over N files, which no candidate
automates), how a *pre-existing* homelab's vault password is imported at bootstrap, and the
interaction with `become_pass`/`ansible_ssh_pass` in `host_vars`. TC §6.3 is the closest (derived
per-project password, with a stored `vault_pass` fallback for pre-existing repos) but does not
handle multiple ids. Since ansible-vault is the one secrets mechanism the operator most likely
*already* has, the migration path deserves a section, not a footnote.

**D3. "OpenBao-class options" is read as "OpenBao later", not "use my existing vault now".**
All three defer OpenBao and reserve a `bao://` scheme. None support an operator whose secrets
already live in OpenBao/Vault/1Password at v1 — including the more interesting variant, using an
existing OpenBao Transit key as the *root of trust* instead of a local age identity, which is
exactly what the goal's phrasing suggests and what `terraform-orchestration.md` notes OpenTofu 1.7+
supports natively as a key provider.

**D4. No ansible-galaxy / collection dependency story, anywhere.** Not in the designs and not in
the research. Every real Ansible repo has a `requirements.yml`; who runs `ansible-galaxy install`,
when, into what cache, and with what network access? This directly hits DR ("restore succeeded but
no playbook can run until the Galaxy fetch works") and image size. `community.sops` — which every
candidate depends on — is itself a Galaxy collection.

**D5. No OpenTofu provider-cache or registry-mirror story.** `tofu init` pulls providers per
workspace per run: hundreds of MB, and a hard dependency on registry.opentofu.org *during disaster
recovery*. None of the three mention `TF_PLUGIN_CACHE_DIR`, a filesystem mirror, `.terraform.lock.hcl`
handling across restore, or the disk budget for `.terraform/` directories. A DR runbook that ends
with "run `tofu plan` to verify" (CF §9 step 6) will fail offline.

**D6. No migration/adoption path from an existing homelab.** TC's repo auto-discovery (§11 step 4)
is the only nod. Nobody describes adopting pre-existing OpenTofu state (moving from local/S3 state
to TC's or HY's HTTP backend needs `tofu init -migrate-state` while the *old* backend still works —
a one-way door that deserves a documented, reversible procedure), an existing Semaphore/AWX install,
or an existing restic repository.

### E. Auth, TLS, and remote access

**E1. Two auth systems, no SSO, in CF and HY.** Both reverse-proxy Semaphore's Vue UI (CF at `/`,
HY at `/semaphore/`) while serving their own approval/backup pages from the companion binary. That
is two login systems, two session cookies, and two user databases in one browser origin — and
Semaphore behind a path prefix needs its `WEB_ROOT` configured, which neither mentions. Who is the
"operator" identity that the audit log records when the action arrives through the proxy?

**E2. The approval callback contradicts "unix socket by default, no TLS".** The whole approval UX
depends on a phone tapping an ntfy action button, which requires an HTTP endpoint reachable *from
the phone*. TC defaults to a Unix socket and says TCP is opt-in; HY publishes `127.0.0.1:8080` in
its own Quadlet unit. Neither closes the loop: what is the URL in the notification, who terminates
TLS, where does the certificate come from, and what happens when the phone is off the tailnet?
"Put Tailscale or Caddy in front" is the answer all three give and none specify, yet it is on the
critical path of the flagship feature.

**E3. TC's default ntfy topic is a public, unauthenticated third-party topic.** §11 step 5:
"ntfy server + topic (default: `ntfy.sh/control-<random>`)". On public ntfy.sh, topics are
unauthenticated — anyone who learns the topic name reads every alert *and* receives the single-use
approval URLs, which are bearer capabilities to run playbooks against the fleet. Random topic names
are obscurity, not access control. Default to self-hosted ntfy, or require an ntfy access token,
and never put a capability URL in a message whose transport is unauthenticated. (CF §11 R8 spots
the "action-button URL is a bearer capability" half of this but not the transport half.)

**E4. The Wish/SSH surface is underspecified in all three.** Unaddressed: the SSH *host* key —
if it is not in the backup, every DR triggers `REMOTE HOST IDENTIFICATION HAS CHANGED` for every
operator, which trains people to ignore it; operator public-key enrolment (chicken-and-egg: the TUI
is the enrolment UI, and you need a key to reach the TUI); where `authorized_keys` lives and whether
it is backed up; rate limiting/fail2ban on an internet-adjacent :2222; and port collision with the
host's own sshd. HY's Quadlet is also internally inconsistent — web is bound to `127.0.0.1:8080`
while `PublishPort=2222:2222` exposes the TUI on every interface.

**E5. No token lifecycle in TC's API auth.** §10 says bearer tokens are "argon2id-hashed in
`secret`", but `secret` (§4) has no scope, expiry, revocation, or last-used columns, and `name` is
UNIQUE — so there is one token per name, no rotation without breaking clients, and no way to revoke
a leaked one except deletion. The audit table records `actor` but nothing maps a token to an
operator.

### F. Resource budget and upgrade — the two goals with almost no design

**F1. Only TC states a resource budget, and it measures the wrong thing.** "<60 MB RSS idle, fail
the CI build above 80 MB" (§2, §13.5) is excellent discipline — but it bounds `controld` alone. The
container also holds `ansible-core` (Python), one forked interpreter per managed host during a play,
OpenTofu (which peaks in the hundreds of MB with providers loaded), restic (memory scales with repo
size), plus rclone, sops, age, git, and — in CF and HY — `ansible-navigator` and `pug`. Nobody
states a *container* memory ceiling, an image size budget (that tool list is plausibly 1–1.5 GB),
a disk budget for `.terraform/` and provider caches, a job concurrency limit derived from memory,
or the target hardware. "Single-container, homelab scale, NOT enterprise" needs a number a Pi 4 or
an N100 can be checked against, and a `MemoryMax=` in the Quadlet unit.

**F2. Upgrade and rollback of the control plane is the least-designed area in all three.**
CF gates Semaphore upgrades behind `ctl apply --dry-run` (§11 R5); HY has a `pre_upgrade` value in
its `backup_run.trigger` enum that is never described anywhere else and a `control-self.yml`
runbook; TC has a `self-update` runbook and "forward-only" migrations (§4). Missing from all three:

- **Forward-only migrations make rollback impossible.** TC's restore refuses a snapshot whose
  `schema_version` exceeds the binary (§7.5/§8), and HY does the same (§5.3 step 4). Combined with
  forward-only migrations, once a bad release migrates the DB there is no way back to the previous
  image except restoring a pre-upgrade snapshot — which no design *takes automatically before an
  upgrade*, and which loses everything since.
- **Semaphore's own DB migrations** (CF, HY) are equally one-way, and both designs pin the version
  precisely because upgrades are scary — without saying how one is ever performed.
- **The circularity**: the control plane runs the job that upgrades the control plane, so a failed
  upgrade removes the tool needed to roll it back. `install.sh` is versioned separately for exactly
  this reason in all three, but none of them give it a `--rollback` / `--image-tag` recovery mode
  as a first-class, tested path (HY mentions `--image-tag` only inside a restore failure branch).
- No **upgrade rehearsal** analogous to the (excellent) restore drill: restore last night's
  snapshot into a scratch instance, run the new image's migrations against it, assert, discard.

### G. Multi-host inventory and managed-host lifecycle

**G1. Nobody designs host onboarding.** How does the control plane's SSH key get onto host N+1?
What creates the `ansible_user`, the sudoers entry, the `become_pass` (which needs vault — see D2)?
This is the single most common day-2 operation in a homelab and appears in no candidate.

**G2. `known_hosts` / host key verification is never mentioned.** Ansible's default
`host_key_checking` behaviour, where the trusted host keys live, whether they are backed up, and
what a post-DR "all host keys unknown" state does to the first `ping-all` (CF §9 step 6, HY §9
step 9 both *depend* on that ping succeeding).

**G3. The inventory is assumed to be a single static file for a single homogeneous fleet.**
No candidate addresses hosts reachable only over Tailscale/WireGuard, bastion/jump-host chains,
non-Linux targets homelabs actually run (OPNsense, Synology DSM, ESXi, network gear), or per-host
credentials. TC's per-job single `ssh-agent` key (§6.3) implicitly assumes one key for all hosts.

### H. Observability of the control plane itself

None of the three expose a `/metrics` endpoint, structured logs for shipping, or a health target for
the very Prometheus/Alertmanager they consume events from. The only self-monitoring is the
healthchecks dead-man for backups (D1) and, in HY, a supervisor health probe. A control plane that
ingests alerts but cannot itself be alerted on is a blind spot — particularly for "reactive engine
paused", "approval queue backing up", "backup target stale >48 h" (HY surfaces this in the TUI only),
and "disk 90 % full" (B6).

## Unverified claims worth pinning before any of this is costed

1. **Semaphore's embedded DB** — BoltDB or SQLite? CF flags it (R2, "the single most load-bearing
   unknown"); HY builds `semaphore.bolt`, a Litestream exclusion, and a restore fallback on the
   unverified answer. Decides whether Litestream applies, whether `VACUUM INTO` applies, and what a
   consistent snapshot even means.
2. **`semaphore project export/import` coverage (HY §5.1, CF R1 exit plan)** — does the v2.17 CLI
   export include the Key Store, schedules, and integrations, or only projects/templates? HY's
   entire "version-independent logical leg" and CF's "rebuild from `control.yaml`" fallback depend
   on the answer. If secrets are excluded (likely, and correct of Semaphore), the logical leg alone
   cannot restore a working instance.
3. **Per-template serialization / concurrency in Semaphore OSS** — CF §3.2 writes `concurrency: 1`
   into `control.yaml` and §2.2 leans on it as the substitute for backend locking. Does the OSS
   build actually offer it?
4. **TC's claim that tofu state is "opaque to the control plane" (§5)** — it is not: `controld`
   derives the PBKDF2 passphrase itself (`HKDF(DEK, "tofu-state:"||workspace.id)`) and injects
   `TF_ENCRYPTION`. Storing the ciphertext in its own table while holding the key is defensible, but
   the stated property is false and should not be used to justify anything.
5. **HY's "Semaphore runs as a separate UID inside the container" (§11.5b)** — the isolation is
   largely notional while `keel` hands Semaphore `SEMAPHORE_ACCESS_KEY_ENCRYPTION` in its
   environment, since that value *is* the key to everything Semaphore holds. Also unstated: how the
   two UIDs share the git working tree and job artifacts on one volume.
6. **HTTP-backend effort estimates (TC "~2 days"/four handlers; HY "~350 LOC")** — the protocol also
   has `DELETE`, custom `LOCK`/`UNLOCK` verbs that some proxies drop, and `-migrate-state`
   interaction (D6). Plausible but untested; both designs treat it as a rounding error.
7. **restic/tofu/ansible-runner `--json` schema stability** — TC §13.3 is the only design that
   treats this as a risk and specifies the correct mitigation (exit code is authoritative, parsing
   only enriches). CF R7 notes it; HY does not.

## Cross-cutting observations

- **TC is the only candidate whose guardrails cannot be bypassed** (it owns the queue), the only
  one with secret redaction, and the only one with a stated resource ceiling enforced in CI. It pays
  for that with the bus-factor risk it names honestly.
- **CF and HY buy time-to-working with an execution engine they do not control**, and both leak the
  same three consequences: bypassable guardrails (C1), no redaction (C3), and two auth systems (E1).
  Whether that is acceptable is a real choice — but it should be made explicitly, not inherited.
- **All three do the hard parts well**: root-of-trust design, the recovery-kit pattern, mandatory
  restore drills, cooldown/cap/dedup/kill-switch guardrails, and the discipline of keeping secrets
  out of UI state. The gaps above are overwhelmingly at the *edges* — credentials to reach a
  backup, retention that reclaims space, upgrade rollback, host onboarding — which is exactly where
  homelab tooling actually fails.
- **The three effort estimates are not comparable** (42 person-days / 22 FTE-weeks / 46–63
  person-days) and none is scoped against the same feature set. Before choosing, restate all three
  against one fixed v1 scope that includes the items in section D.
