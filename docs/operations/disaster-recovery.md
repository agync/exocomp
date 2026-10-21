---
type: Runbook
title: "Disaster recovery"
description: "What is backed up, the three legs, the recovery chain, the runbook for a dead host, the recovery card, failure modes, RPO and RTO, and verification."
status: stable
tags: [operations, recovery, backup]
---

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
match is reported as tampering, a digest that does not match as corruption.

### Application-consistent backups (quiesce hooks)

The snapshot above is consistent for exocomp's own SQLite (VACUUM INTO). For a
*live application's* data (a running Postgres, a busy Vaultwarden SQLite), a raw
copy can be torn. Drop a `.exocomp/backup-quiesce.yml` in the data dir to wrap the
backup in quiesce hooks — its `pre_run` steps quiesce (e.g. `pg_dump`, stop a
compose stack, `sqlite3 .backup`), its `post_run` steps un-quiesce:

```yaml
pre_run:
  - name: dump-postgres
    run: pg_dump -Fc app > /var/lib/exocomp/dumps/app.dump
post_run:
  - name: done
    run: 'true'
```

It uses the same schema as `.exocomp/hooks.yml` (a `gate:` is refused — a backup
has no health gate). A **failed `pre_run` aborts the backup before anything is
staged** — never a torn snapshot — and the failure surfaces to be alerted on; the
`post_run` un-quiesce always runs once quiesce succeeded. A missing file (or one
with no steps) runs the backup exactly as before. Both the `backup run` CLI and
the daemon's scheduled backups honor it. Hooks run sandboxed as the run user
(no sudo, no egress — see [run lifecycle](run-lifecycle.md)).

A snapshot with no tag from a target that never authenticated one still restores, with
a loud `legacy backup: integrity unverified` warning. A git target is the exception: it
is age-encrypted only to public recipients, so the DEK-keyed tag is the **only** thing
binding a snapshot to your keyring. A git target that has authenticated once leaves an
`AUTHENTICATED.mac` marker at its root; a later untagged snapshot there is a downgrade —
the forgery an attacker with git write and the public recipients would attempt — and the
restore **refuses** it (`ErrManifestUnauthenticated`) unless you pass
`--allow-unauthenticated` for a snapshot you are certain is yours. Restic targets need no
marker: their repository password is derived from the DEK, so an attacker without it can
neither read nor write the repo.

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
| private GitHub repo | tar of the staging tree, compressed with `zstd` when it is on PATH (the image carries it) and with gzip otherwise, `age`-encrypted, pushed as a **single orphan commit, force-pushed** to a rolling ref window, chunked into 90 MB `.age.partNN` parts with a `sha256` parts manifest (`internal/backup/chunk.go`) that carries its own `.mac` — it is the one file a chunked snapshot exposes outside the age envelope. The leg refuses loudly before pushing if the bare repository's `objects` directory already exceeds 4 GB |

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
([Decision 0003](../decisions/0003-secrets-envelope.md)).

### The image the deployment ran

`RECOVERY.md` at each target root carries `image-ref:` — the ref the daemon read from the
unit's `EXOCOMP_IMAGE` and recorded. `keyring.age` restores state, but nothing in it
restores the image: a local tag lives in the store of the host that can fail. Capture the
image beside the backup, and load it on the new host:

```sh
podman save -o exocomp-image.tar <image-ref>        # on the host that still runs it

# on the new host: load the image, then the Quadlet units and `exocomp init`:
podman load -i exocomp-image.tar   # or `podman pull <image-ref>` with a registry
```

`--image-load` runs `podman load -i` (or `docker load -i` for the Docker tier) before
install or rollback; `--image-ref` names the image the unit should run. Set the unit's
`EXOCOMP_IMAGE` to that ref so the daemon records it and the next `RECOVERY.md` reflects
reality ([containers](containers.md)). A custom image also records its source base digests
in the recovery set ([Decision 0016](../decisions/0016-artifact-carrier-image.md)): a registry tag may
be used to install, but recovery must not need one (invariant 7). `exocomp doctor` prints
the stored ref, or says it was never recorded.

## Runbook: the control host is gone

You have a printed recovery kit and any one backup target.

```mermaid
sequenceDiagram
    actor Op as Operator
    participant NH as New host (bare)
    participant T as Any backup target
    participant C as exocompd

    Op->>NH: recovery.sh --from <target> --identity <key>
    Note over NH: loads podman + the image<br/>NO exocompd running yet
    NH->>Op: prompt: backup target URI?
    Op->>NH: target URI from the kit
    NH->>T: fetch keyring.age (credential supplied by the operator; on the kit)
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
--rotate-host` re-wraps the keyring and bumps its generation. It refuses while
`exocomp serve` is running: the daemon holds the keyring it started with, and the next
backup would write that one back to every target root. Sealed secrets and derived
subkeys survive untouched — only who can unwrap the DEK changes — and the command ends
with the reprint nag.

`exocomp ssh-serve --listen <addr> --authorized-keys <file>` renders the same TUI over SSH
(public-key auth only, no passwords) for a host with no local terminal at hand — a recovery
host reached only over the network, for instance.

## The recovery card

The card is the manual path: the same chain with no exocomp binary anywhere in it, using
only `age`, `openssl`, and `restic`. A git target's card uses `git`, `age` and `zstd`
instead, or `gzip` for a `.tar.gz` snapshot. `exocomp restore` from a git target also
shells out to `zstd` for a `.tar.zst` snapshot and refuses when it is not on PATH, so a
recovery host without the image needs `zstd` installed.

**The card's literal command text is a build artifact, not documentation.** It is rendered
by the kit renderer, and the exact rendered text is **executed in CI** on a clean image
against a fixture backup, with the derived restic password diffed against `exocompd`'s own
derivation. This rule exists because an earlier card did not run as written — the
pipeline fed an `openssl kdf` invocation that does not read stdin, and the `-kdfopt`
spellings were mixed. A DR card whose commands do not run is worse than no card.

Treat the printed card as authoritative over this page. The chain it automates is:

1. `age -d -i <recovery key file> keyring.age` → the 32-byte DEK.
2. `HKDF-SHA256` over the DEK — versioned domain-separation salt, `info` string
   `restic-repo:<target-id>` — 32 bytes, base64-encoded → `RESTIC_PASSWORD`. Both the salt
   and the target id are printed on the card, so the derivation is reproducible by hand.
3. `restic -r <target URI> restore latest --target <data volume>`, using the read-only
   storage credential you supply (printed on the card).
4. Start the service, then `exocomp doctor`.

The card also carries the QR-coded recovery key, the target URIs, the fingerprints of every
active recipient (so the operator can tell which YubiKey), the healthchecks ping URL, the
ansible-pull trust anchor fingerprint where pull mode is enabled, and — where the optional
Litestream sidecar is configured — the **replica URL**, so a rebuild can find the fast-RPO
copy of `exocomp.db` (see below). The replica URL is a locator, not a secret; the replica's
own storage credentials stay in the sealed secret plane, never on the card.

The kit is displayed once and never stored in the database. The bootstrap wizard requires
typing `I HAVE STORED THIS` before it will continue, and `exocomp doctor` warns if the
database has no recovery-kind keyring row.

## Failure modes

| What died | Recovery |
|---|---|
| Host disk | The runbook above |
| Host key, kit intact | The runbook above — the recovery key unwraps the DEK, a new host key is minted and the old one revoked |
| Recovery kit lost, host alive | `exocomp recovery-kit` re-renders the kit structure from what is already stored; it does not mint a new recovery identity, wrap the DEK to it, or revoke the old row |
| Recovery kit lost **and** host key lost | Unrecoverable, by design; the same property any real vault has |
| DB corrupt, backups fine | `exocomp restore --from <kind:uri> --dest <fresh data dir>` restores the newest snapshot — for a restic kind, an older one by id with `--snapshot <id>` (full or unambiguous prefix; a git target keeps exactly one snapshot, so it has no selector). It refuses a destination that already holds a keyring. There is no in-place restore and no startup `PRAGMA integrity_check` gate |
| One backup target unreachable | Others proceed and each gets its own `backup_run` row; there is no per-target alerting or last-success-age view in `doctor` |
| `exocompd` will not build or run at all | The card's manual path uses only `age`, `openssl`, and `restic`, or `git`, `age` and `zstd` for a git target. The bundles are plain git bundles and `exocomp.db` is plain SQLite |

## RPO and RTO

| Configuration | RPO | RTO (dead host → serving) |
|---|---|---|
| Default: DB-only fast path every 15 min, full tree nightly | ≤ 15 min | ~5 min |
| With the optional Litestream sidecar → S3 | ~1 s | ~5 min |

The 15-minute fast path is a `VACUUM INTO` plus a `restic backup` of just `exocomp.db` — a
few hundred KB after dedup, cheap enough to run constantly. Litestream stays optional
because losing 15 minutes of job history is not a data-loss event worth a second process
for most operators.

## Fast-RPO restore (Litestream)

Litestream is the **fast-RPO complement**, not a replacement: restic stays the authoritative,
encrypted DR chain. The two recover different things, and the precedence is deliberate.

| | Litestream | restic |
|---|---|---|
| Recovers | `exocomp.db` **only** | keyring, sealed secrets, database, git bundles, tofu mirrors — **everything** |
| RPO | ~seconds (continuous WAL shipping) | ≤ 15 min (fast path) / nightly (full tree) |
| Encryption | **none** — plaintext WAL frames; the target must be LAN-local or an already-encrypted store | age-wrapped keyring + restic repository password derived from the DEK |
| Authenticity | none in the transport; proven after the fact by the decrypt round-trip below | per-target manifest MAC + keyring-generation check |
| Role | fast-forward the database on an already-recovered host | the root of trust and the whole recovery |

**When to use which.** For a full disaster — dead host, lost host key — recover with restic
(the runbook above): it is the only chain that carries the keyring and secrets, and Litestream
cannot stand in for it. Then, *if* seconds matter and the Litestream sidecar was replicating,
fast-forward the database to its freshest state:

```sh
exocomp restore --from-litestream \
  --dest <data dir the restic restore rebuilt> \
  --identity recovery.key \
  --replica <replica url>          # omitted: read from the litestream.replica_url setting
```

The command shells `litestream restore`. The image carries the binary, and a host install
outside the image needs it on PATH or the command refuses.

The order is load-bearing. Litestream recovers the **database only**, so `--from-litestream`
requires a data dir that already holds `keyring.age` — the keyring is what opens the database's
sealed rows. Onto an empty dir it refuses and points you back at the restic restore. Because a
replica ships unauthenticated, unencrypted bytes, the restored database is **not trusted until a
decrypt round-trip proves it**: the command unwraps the DEK from the keyring beside it and opens
one sealed row, and only then promotes the recovered file over the live one. A database that
fails the round-trip is discarded, and the live one is left untouched.

**The no-encryption caveat.** Litestream does not encrypt. A LAN-local or `file://` replica is
accepted as-is; a plaintext **public remote** (`s3://…`, `sftp://host…`, `gs://…` on a routable
host) is **refused** unless you pass `--i-know-the-replica-is-encrypted`, which asserts the store
itself is encrypted. This mirrors the config side: never point Litestream at a plaintext remote.

**A restore never resumes schedules.** Like the restic restore, `--from-litestream` leaves the
system paused — it writes the database and stops. Starting `exocomp serve` is a deliberate act,
so a half-understood recovery does not immediately fire jobs.

Deterministic unit tests cover the source-selection precedence, the DB-only-vs-full distinction,
the plaintext-remote refusal, and the decrypt round-trip gate without any `litestream` binary;
the shell round-trip is an e2e that skips cleanly where the binary is absent, the same posture as
the tofu and ansible gates.

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

### `backup drill`: the restic/digest/integrity_check slice, today

`exocomp backup drill [--target <name>]` and `exocomp schedule add --kind drill` implement
the restore-and-verify core of the dr-drill above (`internal/backup.RestoreDrill`): restore
the newest snapshot into a throwaway directory, verify every manifest digest, open the
restored database, and run `PRAGMA integrity_check` — then discard the directory whatever
the outcome. Booting a scratch container, asserting row counts, and running a play/plan with
network egress disabled are not yet wired to it; the section above stays the target shape.

The restic password is derived straight from the running envelope (the same HKDF a backup
writes with), so a scheduled drill needs no offline recovery identity and can run unattended
on a cadence. A git target is the exception — it is only ever encrypted to public recipients
— so it refuses live drilling and points at a manual `exocomp restore --identity` instead.

A drilled snapshot that does not check out (`OK: false`) is a **drill finding**: the CLI
reports it and exits nonzero, and a scheduled fire settles the job failed, which the ordinary
job-failure paging already covers. A target the drill could not even reach — unresolved, no
backup yet — is a plain error, the same finding-vs-infra-error split `RestoreDrill` documents.
