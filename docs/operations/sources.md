# Sources, inventories, and the credential plane

How a run's tree arrives, what inventory it targets, and how it authenticates to the
outside world. The model is [ADR 0010](../adr/0010-source-and-credential-model.md); the
credential binding is the convergent must-fix from
[the expert reviews](../design/expert-reviews.md).

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
ref before a sync advances it, so a bad commit cannot reach the fleet.

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
