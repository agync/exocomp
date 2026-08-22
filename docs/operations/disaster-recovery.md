# Disaster recovery

The design's headline property: **a printed kit plus any one backup target recovers
everything, with no vendor, no internet, and no exocomp code required.** This page is the
runbook and the chain behind it.

## What is backed up

One staging tree, assembled to tmpfs, sent as identical bytes to all three legs:

```
backup-<ts>/
├── MANIFEST.json     manifest/schema/exocomp/keyring versions, tool versions,
│                     timestamp, sha256 + size of every file
├── MANIFEST.mac      HMAC-SHA256 of MANIFEST.json under
│                     Derive("backup-manifest-mac", <target>) — written per leg
├── snapshot.db       ← VACUUM INTO (consistent, WAL-checkpointed, sub-second)
├── keyring.version   the keyring generation this snapshot was written under
└── keyring.age       the N age-wrapped DEK copies — see below
```

`MANIFEST.mac` is not listed inside the manifest it authenticates, and its key is
per target, so a manifest lifted from one target does not verify against another.
A restore checks the tag **before** it trusts a single sha256: a tag that does not
match is reported as tampering, a digest that does not match as corruption. A
snapshot with no tag — every backup written before this shipped — still restores,
with a loud `legacy backup: integrity unverified` warning.

The engine (`internal/backup`) ships exactly this tree. `config.toml`, `blobs/`
(job artifacts), and `repos/*.bundle` (project git bundles) join the tree when those
features land — the design remains as above; the implementation is authoritative for
what exists today.

OpenTofu state ciphertext rides inside `exocomp.db`. ansible-vault and sops files ride
inside the git bundles as ciphertext — restic treats them as opaque blobs, so there is no
extra integration. Provider mirrors, vendored Galaxy collections, and the volume toolchain
ride in the same snapshot, which is what makes an offline restore possible.

## Three legs, independently

| Leg | Mechanism |
|---|---|
| NAS | `restic` to a local repository on the SMB/NFS mount, after asserting mountpoint, canary file, and free space |
| S3-compatible | `restic` to an `s3:` repository. Credentials resolve from sealed secret rows `s3/<target>/access-key-id` and `s3/<target>/secret-access-key` (optionally `.../region`, `.../session-token`) when both required rows are sealed; otherwise the leg falls back to the process environment and logs a warning — it never fails silently on a missing sealed credential |
| private GitHub repo | tar+zstd of the staging tree, `age`-encrypted, pushed as a **single orphan commit, force-pushed** to a rolling ref window, chunked into 90 MB `.age.partNN` parts with a `sha256` parts manifest (`internal/backup/chunk.go`) that carries its own `.mac` — it is the one file a chunked snapshot exposes outside the age envelope. The leg refuses loudly before pushing if the bare repository's `objects` directory already exceeds 4 GB |

One leg failing does not abort the others; each gets its own `backup_run` row and its own
alert. A target that fails twice consecutively raises a `critical` notification. The
dead-man's-switch ping fires **only on full success**, because the failure mode that
actually kills you is silent non-execution.

A `backup_run` row ends in `succeeded`, `failed`, or `cancelled`; a leg the daemon's
shutdown stopped before its drain grace ran out records `cancelled`, not `failed`. The
row is closed whatever happens to the leg, so a row still reading `running` means the
process writing it was killed outright (`SIGKILL`), not that a backup is in progress.

### Sealing S3 credentials

A `restic_s3` target's credentials are ordinary sealed secrets, named
`s3/<target>/<field>` (`<target>` is the name given to `backup add-target`):

```
printf '%s' "$ACCESS_KEY" | exocomp secrets add s3/offsite/access-key-id --kind s3-credential
printf '%s' "$SECRET_KEY" | exocomp secrets add s3/offsite/secret-access-key --kind s3-credential
```

`region` and `session-token` fields are optional, sealed the same way. Both
`access-key-id` and `secret-access-key` must be sealed for the target to use them: a
target with only one, or neither, falls back to the process environment
(`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`) and the run logs a warning — it never fails
silently on a missing sealed credential, and it never mixes one sealed field with one
environment field.

## The one detail that makes recovery work

**`keyring.age` is written to every target root, outside the restic repository**, with
`keyring.version` beside it.

It is ciphertext — N age-wrapped copies of the DEK — so leaving it in the clear is safe. It
must not be *inside* the encrypted backup, because the restic password is derived from the
DEK that the keyring wraps. Putting it inside would be a perfect circular dependency and
the single most likely way to build an unrecoverable system.

```
printed recovery key → fetch keyring.age from ANY target → unwrap DEK
                     → HKDF → restic password → restore → running control plane
```

The generation beside it is what makes a rollback visible: an old `keyring.age` still
unwraps the DEK — that is what it is for — so nothing else distinguishes the current
keyring from one that predates a revoked recipient. `exocomp restore` refuses a keyring
older than the generation the snapshot's manifest records, at the target root and inside
the snapshot alike. Recipient changes and host-key rotation bump the generation
(`exocomp identity migrate`), which is also when the printed card goes stale.

Every arrow is executable with off-the-shelf tools. The second circularity — needing a
storage credential to fetch anything at all — is closed by putting a **read-only** storage
credential per off-host leg on the printed kit itself
([ADR 0003](../adr/0003-secrets-envelope.md)).

## Runbook: the control host is gone

You have a printed recovery kit and any one backup target.

```mermaid
sequenceDiagram
    actor Op as Operator
    participant NH as New host (bare)
    participant T as Any backup target
    participant C as exocompd

    Op->>NH: install.sh --restore
    Note over NH: installs podman + pulls the image<br/>NO exocompd running yet
    NH->>Op: prompt: backup target URI?
    Op->>NH: target URI from the kit
    NH->>T: fetch keyring.age (credential supplied by the operator; on the kit from M2)
    NH->>Op: prompt: paste or scan the recovery age key
    Op->>NH: AGE-SECRET-KEY-1…
    NH->>NH: age -d keyring.age → DEK
    NH->>NH: HKDF(DEK, "restic-repo:<target-id>") → RESTIC_PASSWORD
    NH->>T: restic restore latest --target <data volume>
    NH->>NH: install the Quadlet unit, enable it
    C->>C: boot, try to unwrap the DEK via the host key… ABSENT
    C->>Op: "no host identity — rewrap now?"
    Op->>C: yes
    C->>C: mint a new host identity, wrap the DEK, TPM-seal, add a keyring row
    C->>C: revoke the old host recipient, rebuild schedules from the DB
    C->>Op: N runbooks, M schedules, K workspaces, last job <ts>
```

Then verify rather than assume:

```bash
exocomp doctor       # tool versions, data dir mode, keyring recipients
exocomp tui          # last known activity — does it match your memory?
```

## Deriving without the daemon

The card's rule is one command, and it runs with or without a live envelope:

```bash
exocomp kit derive restic/nas                    # the repository password
exocomp kit derive ansible-vault/homelab         # the vault password for a project
exocomp kit derive --list                        # every purpose:id the store knows of

# Offline: no daemon, no host key — the recovery key and a keyring.age
# fetched from any target root are the only inputs.
exocomp kit derive restic/nas --identity recovery.key --keyring ./keyring.age
```

`exocomp kit verify --page kit.txt` re-renders the kit as it stands now and compares it
with a printed page: a page that no longer hashes to the digest it prints was edited, and
a page that no longer matches the current kit needs reprinting. The card asks for this
quarterly.

`exocomp identity migrate --add-recipient label=age1… | --drop-recipient label |
--rotate-host` re-wraps the keyring and bumps its generation. Sealed secrets and derived
subkeys survive untouched — only who can unwrap the DEK changes — and the command ends
with the reprint nag.

`exocomp ssh-serve --listen <addr> --authorized-keys <file>` renders the same TUI over SSH
(public-key auth only, no passwords) for a host with no local terminal at hand — a recovery
host reached only over the network, for instance.

## The recovery card

The card is the manual path: the same chain with no exocomp binary anywhere in it, using
only `age`, `openssl`, and `restic`.

**The card's literal command text is a build artifact, not documentation.** It is rendered
by the kit renderer, and the exact rendered text is **executed in CI** on a clean image
against a fixture backup, with the derived restic password diffed against `exocompd`'s own
derivation. This rule exists because the pre-amendment card did not run as written — the
pipeline fed an `openssl kdf` invocation that does not read stdin, and the `-kdfopt`
spellings were mixed. A DR card whose commands do not run is worse than no card.

Treat the printed card as authoritative over this page. The chain it automates is:

1. `age -d -i <recovery key file> keyring.age` → the 32-byte DEK.
2. `HKDF-SHA256` over the DEK — versioned domain-separation salt, `info` string
   `restic-repo:<target-id>` — 32 bytes, base64-encoded → `RESTIC_PASSWORD`. Both the salt
   and the target id are printed on the card, so the derivation is reproducible by hand.
3. `restic -r <target URI> restore latest --target <data volume>`, using the read-only
   storage credential you supply (printed on the card from M2, amendment 1).
4. Start the service, then `exocomp doctor`.

The card also carries the QR-coded recovery key, the target URIs, the fingerprints of every
active recipient (so the operator can tell which YubiKey), the healthchecks ping URL, and
the ansible-pull trust anchor fingerprint where pull mode is enabled.

The kit is displayed once and never stored in the database. The bootstrap wizard requires
typing `I HAVE STORED THIS` before it will continue, and `exocomp doctor` warns if the
database has no recovery-kind keyring row.

## Failure modes

| What died | Recovery |
|---|---|
| Host disk | The runbook above |
| Host key, kit intact | The runbook above — the recovery key unwraps the DEK, a new host key is minted and the old one revoked |
| Recovery kit lost, host alive | **M3.** `exocomp recovery-kit` only re-renders the kit structure from what is already stored; it does not mint a new recovery identity, wrap the DEK to it, or revoke the old row. Re-keying and revocation are re-evaluated for M3 |
| Recovery kit lost **and** host key lost | Unrecoverable, by design; the same property any real vault has. The weekly single-recipient nag and the bootstrap wizard are still unbuilt |
| DB corrupt, backups fine | `exocomp restore --from <kind:uri> --dest <fresh data dir>` restores the newest snapshot and refuses a destination that already holds a keyring. M1 has no `--snapshot <id>` selector or in-place restore, and startup `PRAGMA integrity_check` gating is still unbuilt |
| One backup target unreachable | Others proceed and each gets its own `backup_run` row; per-target alerting and the doctor's last-success-age view are **M3** |
| `exocompd` will not build or run at all | The card's manual path uses only `age`, `openssl`, and `restic`. The bundles are plain git bundles and `exocomp.db` is plain SQLite |

## RPO and RTO

| Configuration | RPO | RTO (dead host → serving) |
|---|---|---|
| Default: DB-only fast path every 15 min, full tree nightly | ≤ 15 min | ~5 min |
| With the optional Litestream sidecar → S3 | ~1 s | ~5 min |

The 15-minute fast path is a `VACUUM INTO` plus a `restic backup` of just `exocomp.db` — a
few hundred KB after dedup, cheap enough to run constantly. Litestream stays optional
because losing 15 minutes of job history is not a data-loss event worth a second process
for most operators.

## Verification — the part people skip

- `restic check --read-data-subset=1/12` weekly per target, rotating through the whole
  repository quarterly so bandwidth stays sane.
- A monthly **`dr-drill`**: restore the newest snapshot into a throwaway container, boot
  against it, assert row counts, assert a canary secret round-trips, assert that a play and
  a plan run **with network egress disabled**, then destroy it and post the result. Every
  restored snapshot contains interrupted jobs and stale locks by construction, so boot-time
  reconciliation is asserted here too.
- The drill runs **from a scratch container holding only the kit contents** — no host state,
  no shell history — which is what makes it a real test of the credential chain.
- A scheduled **upgrade rehearsal** mirrors it: restore last night's snapshot into a
  scratch instance, run the new image's migrations against it, assert, discard.

Making the drill a scheduled job in the same engine costs nothing new, and it means a
silently broken envelope fails a routine job instead of being discovered during a real
disaster.
