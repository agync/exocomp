---
type: Guide
title: "Sources, inventories and the credential plane"
description: "Registering git, host and volume sources, inventories and the run-secret credential binding, plus the fleet key and host onboarding."
status: stable
tags: [operations, sources, credentials]
---

# Sources, inventories, and the credential plane

How a run's tree arrives, what inventory it targets, and how it authenticates to the
outside world. The model is [Decision 0010](../decisions/0010-source-and-credential-model.md).

## Sources — two localities

A source is where a run's tree comes from. The credential planes below are identical
across both; only how the tree arrives and stays fresh differs.

| Kind | Register | Freshness |
|---|---|---|
| `git` | `exocomp source add <name> --url git@forge:org/repo.git --ref main` | Auto-synced, pinned to a ref a gated run advances; generates a sealed deploy key |
| `path` | `exocomp source add <name> --path <dir>` | A tree already on the box: an absolute path, or a path relative to the data volume. Read-only by default; `--writable` grants the run a read+write working directory |

`--path` takes one argument. An absolute path is used as-is; a relative path resolves
against the data volume at run time (so a bare `--path checkouts/app` means
`<data-dir>/checkouts/app`), and a relative path may not contain `..`. A path source is
read-only unless registered `--writable` — the common case, running playbooks out of a
tree, needs only read, and the read-only default is the least-privilege posture the
sandbox actually enforces (a read-only tree is granted read, never write).

A git source prints an ed25519 deploy key to add to the forge as a read-only key;
`source sync` (or the Web UI's **Sync now** button on `/sources` and `/sources/{name}`)
clones and pins it. `--signed` requires a verified signature on the pinned
ref before a sync advances it, so a bad commit cannot reach the fleet. Signing trust is
**exocomp's own, never the host git config**: `--signed` must be paired with
`--allowed-signers <file>` or one or more `--signer '<principal> <type> <key>'` lines, and
is refused at `add` time without them. A `--signed` sync with no recorded signers fails
closed rather than falling back to whatever `~/.gitconfig` happens to trust — the fallback
that once let an attacker-signed ref through.

A `git` source over `ssh://` (or `user@host:path`) **pins the forge host key** from the
recorded `known_host` store: when the forge host has a recorded key, `source sync` verifies
it strictly (`UserKnownHostsFile` + `StrictHostKeyChecking=yes`), so a changed or attacker
key fails the sync instead of being trusted on first contact. A forge not yet
recorded stays trust-on-first-use — record its key (`host onboard`, or `ssh-keyscan` into
the store) so the next sync pins it.

**Keep a source tree outside the data dir.** A run's tree is granted read (read-only
sources) or read+write (`--writable` sources) into the sandbox, while the data dir —
`exocomp.db`, `host.key`, `keyring.age` — is denied to every job. Those two rules collide
if a source path is an *ancestor* of the data dir: the sandbox refuses to grant a directory
that contains the keys it must also deny, so the run fails to start. A relative path source
lives in its own subtree of the data volume (e.g. `/var/lib/exocomp/sources/…`, where
`source add` clones git sources) and is structurally guaranteed to stay under it; an
absolute path source is any tree that is not a parent of the data dir. Put playbooks and
workspaces in their own directory, not the one holding the database and keys.

## Source-scoped egress default

An ansible run under the enforcing sandbox reaches TCP 22 and whatever ports it declares.
When a source's runs always need the same extra port — a controller API on 443, a metrics
endpoint on 9090 — record it on the source instead of repeating it on every run
([Decision 0023](../decisions/0023-source-egress-defaults.md)):

```
exocomp source egress homelab 443 9090
```

`source egress <name> [port...]` sets the source's default, replacing any previous set;
with no ports it clears the default. The set is validated (1..65535), de-duplicated, and
sorted. Every ansible run that names the source inherits it at queue admission, unioned
with the run's own `--egress` grant and the ansible kind's built-in SSH port 22 — the union
is what the job row records, so the effective grant is what an approver and the job detail
show.

The default is **operator-set through the daemon only**: nothing a repo declares widens it,
so a checked-out playbook cannot grant itself a port. It is visible on the source's detail
page — an **Egress ports** line, and, for an operator, a form POSTing to
`/sources/{name}/egress` that is the web face of the same verb.

Like every egress grant, it is **a port, not a destination**: the sandbox filters by port,
so a granted port is reachable to any host, not only the intended one.

## The credential plane — sealed secrets as run env

A dynamic-inventory plugin or an OpenTofu provider authenticates from environment
variables. The sandbox env allowlist strips those by default, so a run must bind them
explicitly. A **run-secret set** maps env names to sealed secrets:

```
exocomp secrets add proxmox/token         # seal the value once
exocomp run-secret add prod PROXMOX_PASSWORD proxmox/token
exocomp run-secret add prod PROXMOX_URL proxmox/url
```

Bind the set to a run with `--run-secrets`:

```
exocomp playbook run site.yml --inventory pve --run-secrets prod
exocomp workspace plan prod --run-secrets prod
```

At execution the named set is resolved from the envelope, injected as env for that job
alone, and scrubbed from events by the redactor. Only the binding name travels on the job
row — never the value.

### Minting, replacing, and removing a secret

```
exocomp secrets add proxmox/token --generate --expires 720h
exocomp secrets replace proxmox/token --expires 720h
exocomp secrets rm proxmox/token
```

`secrets add` seals a value read from stdin by default; `--generate` mints one instead (32
random bytes, base64url-encoded) rather than asking the operator to invent one. `--kind`
tags the secret for the AAD (default `generic`); `--expires` takes a Go duration (`720h`) or
an RFC3339 timestamp and is recorded as an absolute instant.

`secrets replace <name>` (with `rotate` kept as an alias) reseals the value under the
unchanged `name|kind` AAD, so every existing binding still opens it. A **generated** secret
(or `replace --generate`) mints a fresh random value automatically — nothing upstream holds
the old one to re-enter, so this is a true rotation; a **user** secret reads its replacement
from stdin and prints a reminder that the previous value may still be live upstream and
revoking it there is the operator's job — the sealed copy is replaced, but the credential
itself is not. `--expires` on `replace` re-sets the expiry (an empty value clears it);
omitting the flag leaves the existing expiry untouched.

`secrets rm <name>` is guarded: a secret still referenced by one or more `run-secret`
bindings is refused unless `--assume-yes` (or `--force`, deprecated) forces past the
binding list, which is printed first so nothing is deleted blind.

**Expiry warns, it never blocks.** `secrets list` renders an `EXPIRED` / `expires in Nd`
hygiene marker computed against the current time, and the TUI and web surfaces render the
same marker as a chip — but expiry gates nothing in the envelope: a sealed value past its
`expires_at` still decrypts exactly as before. A run that resolves a `run-secret` binding
pointing at an expired secret still gets the value; the daemon logs a warning naming the
secret and its expiry instant (never the value) and injects it anyway. Treat expiry as an
operator-facing rotation reminder, not an access control.

## Inventories — static and dynamic

Register an inventory so a run targets it by name:

```
exocomp inventory add hosts --kind static --path inventory/hosts.ini --source repo
exocomp inventory add pve --kind dynamic --path inventory/pve.yml --bind PROXMOX_URL=proxmox/url
```

A dynamic inventory's `--bind ENV=secret` pairs are resolved into the run's env alongside
any `--run-secrets` set, so the plugin authenticates under the sandbox. `playbook run
--inventory <name>` accepts a registered inventory or a plain path.

## Managed-host SSH — fleet key, onboarding, known_hosts

> **v1 runtime status.** The fleet key is generated, sealed, and its public half
> printed, and `host onboard` records each host's key into a backed-up
> `known_host` store — but a run does **not yet** load the fleet key into a
> per-job ssh-agent or verify against the recorded `known_hosts`. For v1, give a
> play its connection credentials the ordinary Ansible way: a vaulted
> `ansible_ssh_private_key_file` or `ansible_password` in `group_vars` (the
> secret lives in the repo vault, decrypted by the registered vault-id). The
> agent-load + `known_hosts` verification runtime is v1.1.

exocomp holds one ed25519 **fleet key** sealed in the envelope. Onboard a host with:

```
exocomp host onboard node0 node1
```

This `ssh-keyscan`s each host and records its keys in the backed-up `known_host` store,
ensures the fleet key exists (generating it on first run), and seeds the onboarding
runbook. It prints the fleet public key to authorize and the exact `playbook run` that
pushes it, creates the automation user, and grants scoped sudo — through the sandboxed,
gated queue. `exocomp host known-hosts` prints the recorded `known_hosts` file; because
the rows live in the store, they travel with the backup rather than being a loose file to
lose.

Connection secrets (`ansible_become_password`, `ansible_password`) come from a vaulted
`group_vars` file in the repo — the secret in the repo vault, the vault password from the
envelope — registered per project:

```
exocomp secrets add ansible-vault/homelab     # or derive it
exocomp playbook run site.yml --vault-project homelab
```

## Using a non-fleet identity for one source or subset

exocomp mints and seals **one** fleet key (`fleet/ssh`) and that key is what
`host onboard` pushes into a node's `authorized_keys`. There is no per-source or
per-host key factory: if you want a separate identity for one source, one
inventory, or a subset of hosts, bring the key yourself. Treat it like any other
operator-supplied connection credential — exocomp seals nothing new and pushes
nothing for you.

1. **Generate the key outside exocomp** (or accept one from the identity provider
   that issues it), and keep the private file **off the source checkout**. A key
   committed to, or generated inside, a (read-only) source tree will be readable
   but not the usual place for controller secrets, and a read-only checkout is not
   granted writes.

2. **Reference it the ordinary Ansible way** in that source's inventory or
   `group_vars`:
   ```
   ansible_ssh_private_key_file: /run/keys/sources/infra-deploy
   ```
   For passwords rather than keys, use `ansible_password` / `ansible_ssh_pass`
   backed by a vaulted variable and `--vault-project <project>`.

3. **Grant the sandbox read access to it.** The key lives outside the checkout, so
   a default-deny ansible job cannot open it. Declare the absolute path on the
   run:
   ```
   exocomp playbook run site.yml --source infra --ssh-identity /run/keys/sources/infra-deploy
   ```
   or bake it into a recurring rule:
   ```
   exocomp rule add deploy-infra --expr '...' --action playbook \
     --repo-source infra --playbook site.yml --ssh-identity /run/keys/sources/infra-deploy
   ```
   `--ssh-identity` is per job and defaults to nothing declared, takes a list,
   names **single files only** (never `~/.ssh` or a directory), requires an
   absolute (or working-directory-relative) path, and rejects a declaration that
   covers the data dir or one of its state files. Keep the key outside the data
   dir, as [the serve page](serve.md#read-grants-for-ssh-identity-files)
   explains. A `--privileged` (unsandboxed) run ignores the
   grant because the whole box is visible; use that only for trusted work.

4. **Authorize the public half yourself.** Either run a playbook that writes the
   key into `~/.ssh/authorized_keys` for the target nodes, or pre-stage it out
   of band. The `host onboard` runbook authorizes the fleet public key only.

5. **Host verification still rides on v1's caveat.** exocomp records each host key
   during `host onboard` and backs it up, but does **not** yet wire the recorded
   `known_hosts` into a run or load the fleet (or any other) key into a
   per-job ssh-agent (v1.1). Until then a run connects with whatever its
   inventory, vaulted variables, or `--ssh-identity` path declares, and Ansible's
   own `host_key_checking` applies. Do not assume a recorded pin is enforced.

The combination above is the documented escape hatch for a dedicated key today;
there is no `exocomp source add --ssh-key` or secret-to-file promotion step that
mints one for you. If the key must rotate, reissue it where it is minted, then
replace the file and (if stored in the envelope) the sealed copy — exocomp
replaces sealed material but cannot revoke what lives upstream.

## OpenTofu state backends

The web UI presents these under "Workspaces" as managed OpenTofu state (with a paste-ready backend config); the CLI verb and
the `workspace` store/API stay as they are.

Two first-class modes ([Decision 0010](../decisions/0010-source-and-credential-model.md)):

- **Mode A — state store only.** `exocomp workspace add <name> --state-only` emits a
  ready-to-paste `backend "http"` block plus its bearer. An unmodified external `tofu`
  (laptop, CI) keeps its state encrypted and locked in exocomp while running plans itself.
  One exocomp workspace = one state = one backend URL; `tofu workspace new`/`select` is
  unsupported against this backend — use a separate exocomp workspace per environment.
- **Mode B — run in server.** `workspace add <name> --dir <root>`, then `workspace
  plan`/`apply` through the queue against exocomp's own backend, optionally gated.

State history is kept as ordinary rows. `exocomp workspace prune [name] --keep N --days M`
drops old serials past the keep policy: the newest N serials and everything within the
last M days are retained, and the current state is never removed.

Related: [Running the daemon](serve.md) · [Offline caches](offline-caches.md) ·
[LAN access](lan-access.md).
