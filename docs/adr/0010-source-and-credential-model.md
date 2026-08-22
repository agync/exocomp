# ADR 0010 — Source and credential model (git-centric, real usage)

Status: accepted · 2026-08-22

## Context

M1/M2 shipped `playbook run <local-path>` and `workspace add --dir <local-dir>` —
runs point at a path on the box. That is a demo shape, not how Ansible and
OpenTofu are actually operated. Real usage is git-source-centric and has three
credential planes the current design under-specifies: git access, secret
decryption, and reaching the managed fleet. The persona review already flagged
fragments (inventory undesigned; host onboarding; per-host become/ssh).

## Decision — the real flows

### Ansible

1. **Source is a registered git repo**, not a path. `exocomp source add
   <name> --url git@forge:org/repo.git [--ref main] [--signed]`. exocomp
   generates an ed25519 **deploy key**, prints the public half to add to the
   forge (or `--key <sealed-secret>` mounts an existing one, sealed into the
   envelope). It clones into the data volume and **auto-syncs** — on a schedule
   and/or a push webhook — pinning to a ref/tag the control plane advances only
   after a gated run (the ansible-pull trust anchor from amendment 3).
2. **Vault is registered, not passed.** Vaulted files live in the repo; the
   operator registers the vault-id password (`exocomp secrets add
   ansible-vault/<project>` or derives it) mapped to a `--vault-id label`.
   Multiple ids per repo (dev/prod) are first-class.
3. **Managed-host SSH is a credential plane.** exocomp holds an ed25519 fleet
   key sealed in the envelope. *Runtime status (v1):* the key is generated and
   sealed and `host onboard` records each host's key into a backed-up
   known_hosts store, but a run does not yet load the fleet key into a per-job
   ssh-agent nor set `UserKnownHostsFile`/`StrictHostKeyChecking` from that
   store — the v1 connection path is a vaulted `ansible_ssh_private_key_file` /
   `ansible_password` in group_vars (secret in the repo vault, decrypted by the
   envelope). The agent-load + known_hosts-verify runtime is v1.1. As designed,
   a per-job `ssh-agent` loads it from memory
   (never disk). Hosts are onboarded by a seeded runbook (push key, create
   `ansible_user`, sudoers, record `known_hosts`); `known_hosts` is stored and
   backed up. `become_pass`/`ansible_ssh_pass` come from sealed rows bound per
   inventory group, never plaintext in the repo. Connection matrix documented
   (Linux/SSH, network_cli, ProxyJump; non-SSH appliances out of scope).
4. **Run** executes from the synced checkout with the vault-id, the fleet key,
   sandboxed, redacted, through the one queue.

### Source locality — three kinds, git is the headline not the only one

A source is one of: a **registered git repo** (headline — auto-synced, signed,
deploy-keyed, as above); a **host path** bind-mounted into the container (the
operator already keeps a repo checkout on the box or on the NAS and points
exocomp at it — no deploy key, sync is the operator's own `git pull` or an
editor); or an **in-container path** in the data volume (seeded by a runbook, or
an operator working directly). The credential planes (vault, fleet key, provider
env) are identical across all three — only *how the tree arrives and stays fresh*
differs. Host and in-container paths are the quick/air-gapped path and what the
tests use; git is the managed path. The sandbox profile grants the source tree
read (host bind mounts are read-only by default); `source add --path <p>` and
`--url <git>` are the two registration forms, and `--dir` on a run is shorthand
for an ad-hoc in-container path.

### OpenTofu — two modes, both first-class

- **Mode A · secure state store only.** `exocomp workspace add <name>
  --state-only` emits a ready-to-paste `backend "http"` block plus its bearer;
  the operator runs `tofu` themselves (laptop, CI) and state lands encrypted
  and locked in exocomp's SQLite. exocomp never runs the plan. This is the
  lowest-commitment on-ramp and must be a documented, first-class path — it is
  the "I trust you with my state, not my applies yet" adoption door.
- **Mode B · run in server.** Source in a registered repo (or `--dir` for
  local), exocomp runs plan/apply through the queue against its own backend,
  the plan is a saved encrypted artifact, apply consumes exactly it, optionally
  approval-gated. State and runs both live in exocomp.

## Consequences

New surface, folded into M3b (`source add|list|sync|rm`, deploy-key + fleet-key
generation, `known_hosts` store, the host-onboarding runbook, `workspace add
--state-only` + backend-config emit) and the inventory/adoption docs. The
`repo` table sketched in M1 is realised here. The local-path forms stay for
quick use and tests but are no longer the headline flow. This is a scope
increase over the original M3b brief and is accepted as correctness, not
gold-plating: the tool is not usable for real Ansible/Terraform without it.
