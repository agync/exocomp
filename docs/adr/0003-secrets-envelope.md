# ADR 0003 — Secrets envelope: age recipients → DEK → HKDF subkeys

## Status

Accepted · 2026-08-21 · extended 2026-08-22: the keyring carries a monotonic generation
(`keyring.version` beside `keyring.age`), recipient changes and host-key rotation bump it
(`exocomp identity migrate`), and a restore refuses a keyring older than the generation the
snapshot's manifest records. One further subkey, `backup-manifest-mac:<target>`,
authenticates `MANIFEST.json` and the git leg's parts manifest.

## Context

One root of trust has to feed every consumer in the system: ansible-vault passwords, sops
file decryption, OpenTofu state passphrases, restic repository passwords, webhook HMAC
keys, and approval-token keys. The stated constraint is that a single printed key recovers
everything, with no vendor and no project code required on the recovery path.

Two failure modes shaped the design:

- **Key sprawl.** A recovery kit carrying a separate backup password, a separate state
  passphrase, and a separate vault password is a kit whose entries drift out of sync with
  reality. The k3s failure mode is the general form: do not let the master secret live
  *only* inside the thing it protects.
- **The circular dependency.** All three candidate kits omitted the credentials needed to
  *reach* the backup — restic cannot open an S3 repository without an access key, and the
  key was in the backup. The critique's gap A1 called this out as a literal circle.

Encrypting each secret directly to N age recipients would also work, but adding a
recipient later forces a decrypt/re-encrypt of every row, and each secret carries ~200
bytes of recipient stanzas.

## Decision

**Envelope.** N age recipients each wrap one 32-byte DEK, stored as `keyring` rows. The
default recipients are the host identity and a printed recovery identity; an
`age-plugin-yubikey` recipient is optional. Any one recipient unwraps the DEK. The host
identity's private key (`host.key`) is protected **at rest** so a stolen data volume alone
does not contain it: on the primary systemd tier it is delivered through an encrypted
`systemd-creds` credential (`--with-key=host`, TPM-sealed with `host+tpm2` where a TPM is
present), whose unwrap secret lives outside the data volume; on the compose tier it is a
scrypt-passphrase-wrapped `host.key.age` with the passphrase held outside the volume. A
bare plaintext `host.key` is the insecure fallback only — `init` warns loudly and `doctor`
reports the real tier.

**Secrets under the DEK.** `secret.ciphertext` is AES-256-GCM under the DEK with random
96-bit nonces and `(name, kind)` bound as AAD under a length-prefixed encoding
(`0x01 ‖ len(name) ‖ name ‖ len(kind) ‖ kind`), so no two distinct rows share additional
data and a ciphertext moved to another row fails to open. Rows sealed under the earlier
unescaped `name|kind` AAD still open — decryption falls back to that encoding — but nothing
seals with it any more. Zero novel cryptography: `filippo.io/age` as a library, Go stdlib
GCM and `crypto/hkdf`.

**Everything else is derived.** HKDF-SHA256 from the DEK with a versioned
domain-separation salt and a per-consumer `info` string yields the restic repository
password per target (`info = "restic-repo:<target-id>"`), the OpenTofu state passphrase per
workspace (`"tofu-state:<workspace-id>"`), the webhook HMAC key per source, the approval
token key, and the ansible-vault password per project. The printed kit therefore carries
**one** secret.

**The kit closes the credential loop.** In addition to the recovery age key, the kit
carries a **dedicated read-only storage credential per off-host leg** (an S3 key scoped to
GET/LIST only; a GitHub fine-grained PAT with read-only contents on the backup repo), the
healthchecks dead-man ping URL, and the ansible-pull trust anchor fingerprint if pull mode
is enabled. These are `secret` rows tagged `on_recovery_kit=true`; the kit renderer refuses
to render without them, and rotating one regenerates the kit with a nag until reprinted.

**`keyring.age` lives outside the encrypted backup**, written to every backup target root.
It is ciphertext, so publishing it is safe — and putting it *inside* the restic repository
whose password is derived from the DEK it wraps would be a perfect circle.

## Consequences

- Adding or revoking a recipient is O(1) — one unwrap, one wrap — and touches no secret.
- DEK rotation is the most dangerous operation in the system, because the derived subkeys
  change with it: restic repos must be re-keyed and OpenTofu state re-encrypted.
  `exocompd` sequences the whole thing as a single job with a rollback point, refuses to
  start if any backup target is unreachable, and gates it behind an approval even for the
  operator.
- The security claim is stated honestly and not overstated: `exocompd` can decrypt
  everything it manages. The envelope protects backups and off-host attackers, and a
  stolen disk **when `host.key` is on a secure at-rest tier** (systemd credential or
  passphrase-wrapped) — a bare plaintext `host.key` in the data dir defeats the stolen-disk
  case, which is why that state is a loud warning rather than a supported default. It does
  not protect against root on the box. State ciphertext is opaque *to backup targets*, not
  to the control plane, which derives the passphrase itself.
- The monthly DR drill runs from a scratch container holding **only** the kit contents — no
  host state, no shell history — so the circular-dependency class fails a scheduled test
  instead of a real disaster.
- Losing the recovery kit *and* the host key is unrecoverable by design, the same property
  any real vault has. `exocomp doctor` nags weekly if only one recipient exists, and the
  bootstrap wizard refuses to finish without a second.
- The DEK is pinned into RAM with `mlock(2)` for its lifetime (released and zeroed on
  close), and each derived subkey is mlocked best-effort while it is held; a host that
  forbids mlock (a low `RLIMIT_MEMLOCK`) still runs, and `exocomp doctor` reports the real
  state rather than assuming it. Decrypted values live in that memory and a per-job tmpfs
  directory, never in `exocomp.db` and never in `job_event`; a redaction filter scrubs known
  secret values from every event line before it is persisted.
- The key-provider is an interface. The local age identity is the default implementation;
  an external KMS or OpenBao Transit key can substitute at bootstrap, which lines up with
  OpenTofu's own support for Transit as a state-encryption key provider.

See [disaster recovery](../operations/disaster-recovery.md) for the executable chain.
