---
type: Guide
title: "Bootstrap"
description: "The image and Quadlet deploy, the six-screen idempotent wizard, node-zero self-management, the separately versioned recovery script, and the adoption runbooks for an existing homelab."
status: stable
tags: [operations, bootstrap]
---

# Bootstrap

Target: **under 10 minutes**, dominated by typing a git URL and printing the recovery kit.

## Install

The deployment is the shipped image plus the Quadlet units. Podman Quadlet is
primary — rootless, systemd-native, journald, and a smaller blast radius for a
box holding key material. A compose file ships as the fallback for Docker-only
hosts (Synology, TrueNAS); see
[the platform contract](../architecture/platform-contract.md).

On the host:

1. get the image — `podman pull ghcr.io/agync/exocomp:latest`, or on an air-gapped
   box `podman load -i exocomp-image.tar` from the recovery set,
2. drop `quadlet/exocomp.container` and `quadlet/exocomp.volume` into
   `~/.config/containers/systemd/` and run `loginctl enable-linger`,
3. `systemctl --user daemon-reload && systemctl --user start exocomp`,
4. `exocomp init` (or the `setup` wizard) to create the identity and print the
   recovery kit.

The recovery primitive is deliberately **not** "run the control plane": it is
the separately versioned `recovery.sh` ([invariant 7](https://github.com/agync/exocomp/blob/main/AGENTS.md)), which
rebuilds a data dir from a backup with no exocomp code in the path — delegating
to `exocomp restore` when a binary is present, and running the recovery card's
hand chain (age → openssl HKDF → restic, sha256-verified) when it is not. Run
`recovery.sh --help`. It is versioned separately on purpose: the recovery path
must not depend on the thing that broke.

### Host key at rest

`exocomp init` writes `host.key` under the data volume. Move it into a systemd
credential so a stolen volume holds only the opaque `host.key.cred` that is
useless without the host secret (or the TPM):

```sh
# TPM 2 present (/dev/tpmrm0): seal to the TPM. Without one, --with-key=host
# uses the systemd host secret. Either way the plaintext is removed.
systemd-creds encrypt --name=host.key --with-key=host+tpm2 \
  /var/lib/exocomp/host.key /var/lib/exocomp/host.key.cred
rm -f /var/lib/exocomp/host.key
```

The quadlet's `LoadCredentialEncrypted=host.key:…` decrypts it back to
`$CREDENTIALS_DIRECTORY/host.key` at start. If TPM sealing fails, retry with
`--with-key=host`. This is a documented one-off, not a scripted mode.

## The wizard

The wizard is the TUI's first-run mode. Six screens, each **idempotent** — check before
create, so re-running it never clobbers what exists and never has a one-shot
create-the-admin footgun.

| # | Screen | What happens |
|---|---|---|
| 1 | **Identity** | Generate the host age identity; auto-detect TPM2 and offer `systemd-creds` sealing (default yes); generate the DEK and wrap it. One screen explains the availability-versus-confidentiality trade between a file-backed key, a TPM-sealed key, and a YubiKey-only keyring |
| 2 | **Recovery kit** | Mint the recovery identity, render the printable kit plus QR, require typing `I HAVE STORED THIS`. **The wizard cannot be skipped past this screen.** Optionally add a YubiKey recipient now |
| 3 | **Backup targets** | Add at least one NAS path, S3 URI, or GitHub repo. Each is *tested immediately*: init the restic repo, write a canary, read it back, delete it. A target that fails the round-trip cannot be saved |
| 4 | **First project** | Git URL, with a deploy key generated and displayed for pasting into the forge; fetch, then **auto-discover** — repo-root YAML files with a `hosts:` key become candidate Ansible runbooks, directories containing `*.tf` become candidate OpenTofu workspaces, and the operator checkboxes what to import. Also the import-existing-vault-password flow |
| 5 | **Notifications** | ntfy server and topic — **self-hosted or token-authenticated, never a public topic**, because approval URLs are bearer capabilities — with a test push. healthchecks is provisioned here as a first-class step, and its ping URL is stored, backed up, and printed on the kit |
| 6 | **Verify** | Run `exocomp doctor` (`--deep` arrives with the wizard), run the first backup, and run `dr-drill` **immediately**, showing the result. **You have a proven restore path before you have done any real work** |

Screen 4's auto-discovery is the difference between "five minutes to working" and "an
afternoon of forms". Screen 6 is the one that cannot be made optional: it is the only thing
that turns the recovery chain from a design claim into an observed fact.

## Node zero

The wizard seeds an `exocomp-self` project containing the Quadlet unit, the daemon config,
and a `self-update` runbook, so updating the control plane is a normal approval-gated,
logged, rollback-able job — and every upgrade takes a mandatory pre-upgrade snapshot first
([upgrade and rollback](upgrade.md)).

The *recovery* primitive is deliberately **not** "run the control plane". It is the
separately versioned `recovery.sh`. Self-update is opt-in and never automatic: an unattended
bad update of the thing that manages everything else is a single point of failure for the
whole homelab.

## Adopting an existing homelab

Adoption paths are reversible runbooks tested against fixtures, not one-way doors:

- **Existing OpenTofu state** — `exocomp workspace adopt <name>` runs one
  `tofu init -force-copy` (which implies `-migrate-state`) with an unencrypted
  read fallback so the old `terraform.tfstate` migrates onto the HTTP backend.
  The local file is drained and tofu keeps a `.backup`; the fallback exists for
  that one run only — every later run is encrypted as usual. Reversible: the
  old backend's `.backup` still exists while the migration is fresh.
- **Existing ansible-vault password** — imported at screen 4, stored as a `vault_pass`
  secret and bound to a vault-id.
- **Existing restic repository** — verified and adopted rather than re-initialised.
- **Existing hosts** — the seeded host-onboarding runbook pushes the SSH key, creates the
  `ansible_user` and sudoers entry, and records the host key into the database so that DR
  restores host trust rather than triggering a fleet-wide host-key warning.
