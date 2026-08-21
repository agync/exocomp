# Bootstrap

Target: **under 10 minutes**, dominated by typing a git URL and printing the recovery kit.

## Install

One command fetches the installer from `get.exocomp.dev`. Podman Quadlet is primary —
rootless, systemd-native, journald, and a smaller blast radius for a box holding key
material. A compose file ships as the fallback for Docker-only hosts (Synology, TrueNAS)
and `install.sh` detects the engine and **states which tier it is on** rather than implying
parity; see [the platform contract](../architecture/platform-contract.md).

The installer:

1. drops the `exocomp.container` and `exocomp.volume` Quadlet units into the user's
   systemd configuration and runs `loginctl enable-linger`,
2. reloads and starts the unit,
3. prints how to reach the interface — the local TUI, or SSH to the Wish listener.

`install.sh` is dependency-free POSIX shell needing only `curl` and `podman`, and it is
**versioned separately from `exocompd`**. That separation is deliberate: the recovery path
must not depend on the thing that broke. The same script carries `--restore` and
`--rollback --image-tag <X>`.

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
([amendment 3](../design/amendments.md#3-upgrade-and-rollback-of-the-control-plane-itself)).

The *recovery* primitive is deliberately **not** "run the control plane". It is the
separately versioned `install.sh`. Self-update is opt-in and never automatic: an unattended
bad update of the thing that manages everything else is a single point of failure for the
whole homelab.

## Adopting an existing homelab

Adoption paths are reversible runbooks tested against fixtures, not one-way doors:

- **Existing OpenTofu state** — `tofu init -migrate-state` onto the HTTP backend, with a
  documented back-out performed while the old backend still exists.
- **Existing ansible-vault password** — imported at screen 4, stored as a `vault_pass`
  secret and bound to a vault-id.
- **Existing restic repository** — verified and adopted rather than re-initialised.
- **Existing hosts** — the seeded host-onboarding runbook pushes the SSH key, creates the
  `ansible_user` and sudoers entry, and records the host key into the database so that DR
  restores host trust rather than triggering a fleet-wide host-key warning.
