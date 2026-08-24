# Sources, inventories, and the credential plane

How a run's tree arrives, what inventory it targets, and how it authenticates to the
outside world. The model is [ADR 0010](../adr/0010-source-and-credential-model.md).

## Sources — three localities

A source is where a run's tree comes from. The credential planes below are identical
across all three; only how the tree arrives and stays fresh differs.

| Kind | Register | Freshness |
|---|---|---|
| `git` | `exocomp source add <name> --url git@forge:org/repo.git --ref main` | Auto-synced, pinned to a ref a gated run advances; generates a sealed deploy key |
| `hostpath` | `exocomp source add <name> --path <dir> --kind hostpath` | A tree bind-mounted from the host, read-only by default; the operator's own `git pull` |
| `volpath` | `exocomp source add <name> --path <dir> --kind volpath` | A tree in the data volume, seeded by a runbook or edited in place |

A git source prints an ed25519 deploy key to add to the forge as a read-only key;
`source sync` clones and pins it. `--signed` requires a verified signature on the pinned
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

**Keep a source tree outside the data dir.** A run's tree is granted read (host paths)
or read+write (volume paths) into the sandbox, while the data dir — `exocomp.db`,
`host.key`, `keyring.age` — is denied to every job. Those two rules collide if a source
path is an *ancestor* of the data dir: the sandbox refuses to grant a directory that
contains the keys it must also deny, so the run fails to start. A `volpath` source
therefore lives in its own subtree of the data volume (e.g. `/var/lib/exocomp/sources/…`,
where `source add` clones git sources), never at the data-dir root; a `hostpath` source is
any tree on the host that is not a parent of the data dir. Put playbooks and workspaces in
their own directory, not the one holding the database and keys.

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

### Minting, rotating, and removing a secret

```
exocomp secrets add proxmox/token --generate --expires 720h
exocomp secrets rotate proxmox/token --expires 720h
exocomp secrets rm proxmox/token
```

`secrets add` seals a value read from stdin by default; `--generate` mints one instead (32
random bytes, base64url-encoded) rather than asking the operator to invent one. `--kind`
tags the secret for the AAD (default `generic`); `--expires` takes a Go duration (`720h`) or
an RFC3339 timestamp and is recorded as an absolute instant.

`secrets rotate <name>` reseals the value under the unchanged `name|kind` AAD, so every
existing binding still opens it. A **generated** secret (or `rotate --generate`) mints a
fresh random value automatically — nothing upstream holds the old one to re-enter; a **user**
secret reads its replacement from stdin and prints a reminder that the previous value may
still be live upstream and revoking it there is the operator's job. `--expires` on `rotate`
re-sets the expiry (an empty value clears it); omitting the flag leaves the existing expiry
untouched.

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

## OpenTofu state backends

Two first-class modes ([ADR 0010](../adr/0010-source-and-credential-model.md)):

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
