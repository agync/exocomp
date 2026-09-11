# Provision → onboard → converge

`exocomp provision run` turns "I made a VM" into "it is onboarded, configured,
and stays converged". It extends the [Terraform→Ansible
handoff](terraform-ansible-handoff.md) by the two steps that stand
between a finished `tofu apply` and a machine you can actually run plays
against: waiting for its SSH to come up, and onboarding it.

The whole thing is one step-ledger workflow ([ADR 0008](../adr/0008-step-ledger-over-dbos.md)):
four steps, each releasing the next only on success, resumable from the ledger
after a crash. There is no new driver — the three plays are ordinary
ansible-kind jobs through the one queue.

## The chain

```
exocomp provision run <workspace> <playbook> [--ssh-wait 5m] \
    [--vault-project P] [--run-secrets B] [--onboard-playbook P] \
    [--require-approval] [--wait]
```

`--require-approval` gates the step-1 apply behind a durable operator approval
the daemon honors mid-chain: no host is provisioned until `exocomp approval
decide <id> --approve` releases it, the same gate a single destructive apply
passes through.

| Step | Kind | What it does |
|---|---|---|
| 1 | `tofu-apply` | `tofu apply` plus the output capture: the workspace's declared, **non-sensitive** outputs are materialized as `artifacts/inventory/<workspace>.json` and published as the `tf:<workspace>` inventory |
| 2 | `ssh-wait` | The seeded `wait-ssh.yml` waits, bounded by `--ssh-wait`, for each derived host's SSH port to accept a connection |
| 3 | `host-onboard` | The seeded `onboard.yml` creates the automation user, authorizes the fleet key, and grants passwordless become. The driver pins each host's key first |
| 4 | `ansible-play` | Your configure play, against the same materialized inventory |

Follow it with `exocomp provision status <workflow-id>`, which prints the same
ledger view `handoff status` does.

The chain needs a running `exocomp serve`: its sequencer releases each step, its
workers run the jobs, and it is the daemon — not this command — that resumes the
workflow after a restart. `provision run` refuses when no daemon is up.

### The hosts are not known when the workflow is defined

They are whatever the apply's outputs turn out to name. So steps 2–4 do not
carry a host list: each targets the standing `tf:<workspace>` inventory **by
name** and reads whatever the apply materialized minutes earlier. That
indirection is what lets one durable workflow span "no host exists yet" to "the
host is converged".

Shape the hosts with an `ansible_inventory` output, exactly as for the handoff:

```hcl
output "ansible_inventory" {
  value = {
    all = { children = { web = { hosts = { web1 = {
      ansible_host = hcloud_server.web1.ipv4_address
      ansible_user = "root"
    } } } } }
  }
}
```

`ansible_port` is honored everywhere it matters — the wait probes it, and the
host-key scan records the host under the `[host]:port` name `known_hosts` uses.
A host declared `ansible_connection = "local"` needs neither, and both steps
skip it.

### Waiting is bounded, and failing is the point

An apply returns as soon as the provider says the resource exists; the box
behind it is still booting. Step 2 probes the port from the control plane —
it never connects to the host, because the credentials it would connect with are
what step 3 pushes.

`--ssh-wait` (default 5m) bounds it. A host that never answers **fails step 2**,
which leaves steps 3 and 4 `blocked`: a machine that did not come up is not
onboarded, and no play runs against a half-built fleet.

### The host key is pinned before anything is pushed

Before step 3 runs, the driver `ssh-keyscan`s every derived host and records its
key in the backed-up `known_hosts` store — the same store `exocomp host onboard`
writes, readable with `exocomp host known-hosts`.

Trust on first use is one-way. A host whose recorded key has **changed** fails
the step rather than being re-pinned: that is either a rekey you confirm out of
band (`exocomp host onboard --rotate`) or somebody in the middle, and neither
should silently inherit the fleet key.

The fleet public key travels to the play as a JSON `--extra-vars` blob. Ansible's
`k=v` extra-var form splits on whitespace, and an `authorized_keys` line has two
spaces in it; `exocomp host onboard` prints the JSON form for the same reason.

`--onboard-playbook` replaces the seeded runbook when your hosts need something
else (a different user, a narrower sudoers, an image that already has the user).
It receives the same `exocomp_fleet_pubkey` extra-var.

### Resuming

A daemon killed mid-chain resumes from the ledger on its next boot: a step that
already succeeded is never re-run, and the step the crash interrupted is
re-armed and run again. Every step in this chain is idempotent by construction —
`apply` converges, `wait_for` re-probes, the onboarding play is an Ansible play,
and your configure play should be one too.

## The standing inventory

`tf:<workspace>` is not a one-shot artifact. It is a registered inventory whose
file **every** successful apply of that workspace refreshes — the chain's apply,
`exocomp workspace apply`, a daemon apply, a reactive one. Anything that takes
an inventory by name can target it:

```
exocomp playbook run site.yml --inventory tf:edge
```

### A recurring converge that new hosts join by themselves

```
exocomp schedule add fleet-converge --cron '0 0 3 * * *' \
    --kind play --target tf:edge --concurrency-key fleet-converge \
    -- /srv/play/converge.yml
```

`--kind play` fires an ordinary ansible job that names the standing inventory,
and the daemon's ansible driver hands the run a private 0700 copy of the file —
the same boundary the handoff's play gets, with no bearer, no state passphrase,
and no `tfstate` anywhere near it.

Because the schedule names the inventory rather than a host list, provisioning
another VM is the whole procedure: the apply refreshes `tf:edge`, and the next
tick converges the new host. Nothing about the schedule changes.

`--target` is the inventory name, `--concurrency-key` serializes overlapping
fires, and everything after `--` is the playbook and its passthrough flags
(`--check`, `--limit`, `--tags`).

### It carries the workspace's sealed outputs too

Naming `tf:<workspace>` also resolves the run-secret binding of that name: the
workspace's `sensitive = true` outputs, sealed by the apply and bound to env
names, reach the play as `$DB_PASSWORD` and friends. That holds for step 4 of
this chain, for `playbook run --inventory tf:<workspace>`, and for the recurring
converge above — and for nothing that does not name the inventory. See [sealed
tofu outputs](sealed-tofu-outputs.md).

### It fails closed

A fire whose inventory is unregistered, is not a materialized file, has lost its
file, or **names no hosts** enqueues nothing at all and logs why. A recurring
converge that quietly runs against an empty host list is worse than one that
stops: it reports success forever while converging nobody.

The most common way to get a hostless inventory is declaring no
`ansible_inventory` output — the capture then falls back to exposing every
non-sensitive output as an `all` group var, which is a valid inventory with no
hosts. The apply's own event says which you got:

```
captured 2 output(s) to inventory /…/edge.json naming 0 host(s)
```

## Limits worth knowing

- **Sandbox egress is port 22 unless a job declares more.** The `enforcing` tier
  grants an ansible job outbound TCP on 22 by default and no path outside the
  data dir. A job whose inventory host answers on a nonstandard SSH port
  declares it with `playbook run --egress <port>` (or a playbook rule's
  `--egress`) and the sandbox opens that port too; a job or rule that declares
  nothing keeps port 22 and nothing else. A connection using a key file
  elsewhere on the box still needs the `disabled` tier today
  ([ADR 0005](../adr/0005-job-sandboxing-nono.md)). Cloud VMs on port 22 with
  credentials from the vault or a run-secret are unaffected.
- **The recorded `known_hosts` is not yet what the run verifies against.** The
  pin is recorded and backed up; wiring it into the run as
  `UserKnownHostsFile`, alongside loading the fleet key into a per-job ssh-agent,
  is the ADR 0010 v1.1 runtime item. Until then the play connects with whatever
  its inventory or vault declares.
- **The onboarding runbook needs `ansible.posix`.** Vendor it once with
  `exocomp playbook vendor --requirements requirements.yml` so the step does not
  depend on Galaxy being reachable ([offline caches](offline-caches.md)).

## Proving it

`test/e2e/provision-converge-gate.sh` runs the whole thing against the lima node
(`test/lima`) as the provisioned host, reaching its sshd through a proxy the gate
starts **late** so the wait step waits for something real. It asserts the four
ledger steps, the bounded wait failing on an unreachable host, the pin landing
before the onboarding, the fleet key ending up in the node's `authorized_keys`,
a mid-chain `kill -9` resuming from the ledger, a scheduled play converging a
host a later apply added, and a hostless inventory running no play at all.
