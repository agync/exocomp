---
type: Guide
title: "Bootstrap"
description: "The image and Quadlet deploy, the four-screen first-run wizard, the host-key passphrase tier, the backup drill that proves recovery, and adoption of existing OpenTofu state."
status: stable
tags: [operations, bootstrap]
---

# Bootstrap

Target: **under 10 minutes**, dominated by storing the recovery kit.

## Install

The deployment is the shipped image plus the Quadlet units. Podman Quadlet is
primary — rootless, systemd-native, journald, and a smaller blast radius for a
box holding key material. A compose file ships as the fallback for Docker-only
hosts (Synology, TrueNAS); its first-run sequence and passphrase secret are in
[containers](containers.md#compose-first-run).

On the host:

```bash
# 1. the image, the units, and a user manager that outlives the login
podman pull ghcr.io/agync/exocomp:latest
mkdir -p ~/.config/containers/systemd
cp packaging/quadlet/exocomp.container packaging/quadlet/exocomp.volume \
  ~/.config/containers/systemd/
loginctl enable-linger
systemctl --user daemon-reload

# 2. the data volume and the first web token; serve refuses to bind without one
podman volume create systemd-exocomp
podman run --rm --userns keep-id -v systemd-exocomp:/var/lib/exocomp \
  ghcr.io/agync/exocomp:latest --no-input token new web

# 3. the host-key passphrase, outside the data volume, then the wizard
install -d -m 700 ~/.config/exocomp
openssl rand -base64 32 > ~/.config/exocomp/host-key.passphrase
chmod 600 ~/.config/exocomp/host-key.passphrase
podman run --rm -it --userns keep-id \
  -e EXOCOMP_HOST_KEY_PASSPHRASE_FILE=/run/secrets/host-key.passphrase \
  -v ~/.config/exocomp/host-key.passphrase:/run/secrets/host-key.passphrase:ro,Z \
  -v systemd-exocomp:/var/lib/exocomp \
  ghcr.io/agync/exocomp:latest setup

# 4. start it
systemctl --user start exocomp
```

The volume is named `systemd-exocomp` because the Quadlet prefix is `systemd-`.
Create it before the first container write: a volume first written under a
different user namespace belongs to the wrong uid, and `serve` then fails on
`serve.lock` with permission denied. Compose prefixes its own volume name.

`exocomp setup` needs a terminal (`-it`); it is the first-run wizard described
below. Store the recovery kit it prints before going further.

The rootless unit runs `serve` on the container's `0.0.0.0:8642` behind the
published `127.0.0.1:8642`, and the state/API backend on `127.0.0.1:8643`. Add
`--public-url https://your.host` once the surface has an address a phone can
reach, so approval links and push deep links point at it.

### Host key at rest

The design property is that a stolen data volume alone yields nothing. There are
two ways to hold the key outside the volume, and a rootless **user** unit can
only use the second:

- **systemd credential** (`LoadCredentialEncrypted=` with
  `systemd-creds --with-key=host`). The unwrap secret is root-only, and a user
  manager rejects the resulting file with `Scope mismatch: Wrong medium type`.
  This tier is for a system-level deployment.
- **passphrase-wrapped** (`host.key.age`, the shipped rootless unit). The
  passphrase is a random string in `~/.config/exocomp/host-key.passphrase`,
  outside the volume, passed in as a plain systemd credential
  (`LoadCredential=`) and named to the container with
  `EXOCOMP_HOST_KEY_PASSPHRASE_FILE`. `init` and `setup` write `host.key.age`
  when that variable is set; `serve` unwraps it at open.

`doctor` reports the real tier, and `init` warns loudly if it ever writes a bare
`host.key`. Losing both the recovery kit and the host key is unrecoverable by
design, so the passphrase file is a host secret like any other: keep it with the
host's other credentials, not in the data volume.

## The wizard

The wizard is the TUI's first-run mode: four screens, one write.

| # | Screen | What happens |
|---|---|---|
| 1 | **Operator & identity** | Captures the operator display name, detects the machine name, and generates an `exo-{machine}-{id}` ntfy topic |
| 2 | **Recovery kit** | Renders the kit and requires typing `I HAVE STORED THIS`. The wizard cannot be skipped past this screen |
| 3 | **Mint** | Leaving this screen mints the host identity and keyring, saves the operator and ntfy topic, attempts a test push, and prints the kit. This is the single write; quitting earlier leaves no trace |
| 4 | **Hand over** | Shows the kit again with the notify status |

A bare `exocomp init` does the identity and kit only, without the operator or
ntfy settings; it is the non-interactive form for a scripted host. Neither the
wizard nor `init` takes a backup.

## After the wizard

1. **Prove recovery.** Register a target, take a backup, and drill it:

   ```bash
   exocomp backup add-target --name nas --kind restic_local --uri /mnt/nas/exocomp
   exocomp backup run --target nas
   exocomp backup drill --target nas
   ```

   The drill restores the newest snapshot into a throwaway directory, verifies
   the manifest digests, and opens the restored database for `integrity_check`.
   The live data dir is never touched. [Disaster recovery](disaster-recovery.md)
   covers the three legs and the hand chain.

2. **Add a source.** `exocomp source add <name> --url <git-url>` registers a
   repository and prints a deploy key; `--path` registers a tree already on the
   box. See [sources](../guide/sources.md).

3. **Wire notifications.** The ntfy server URL and token live under the system
   page's push channel; the wizard already minted the topic. See
   [setup, notifications and metrics](notifications.md).

4. **Check the host.** `exocomp doctor` reports the data dir, the host-key tier,
   the sandbox probe, the tool manifest, and the self-observability gauges.

## Recovery

The recovery primitive is deliberately **not** "run the control plane": it is the
separately versioned `recovery.sh`
([invariant 7](https://github.com/agync/exocomp/blob/main/AGENTS.md)), which
rebuilds a data dir from a backup with no exocomp code in the path — delegating
to `exocomp restore` when a binary is present, and running the recovery card's
hand chain when it is not: age → openssl HKDF → restic for a restic target, and
git clone → check the parts manifest and join the `.age.partNN` parts → age →
zstd or gzip → tar for a git target, sha256-verified either way. The git chain
also checks the manifest tags under the target's manifest subkey and refuses a
snapshot without them unless `--allow-unauthenticated` is given. The tags are
keyed from the branch's own `keyring.age`, so `--keyring` with a copy you trust
makes the script refuse a branch whose keyring differs, and `--target-name`
stops the branch choosing which target's tags it passes for. Run
`recovery.sh --help`. It is versioned separately on purpose: the recovery path
must not depend on the thing that broke.

## Adopting existing state

An existing OpenTofu workspace migrates onto the HTTP backend with one command:

```bash
exocomp workspace adopt <name>
```

It runs `tofu init -force-copy` once (`-force-copy` implies `-migrate-state`) with an
unencrypted read fallback so the old `terraform.tfstate` moves, then removes it; tofu
keeps a `.backup`, and every later run is encrypted as usual. Run it where `exocomp serve` is not running —
the adopt run serves the state backend itself. It refuses a workspace that
already has state in exocomp.
