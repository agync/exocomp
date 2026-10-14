---
type: Guide
title: "Provision-to-converge handoffs"
description: "Chaining OpenTofu apply and Ansible playbook executions into a single atomic, resumable ledger workflow."
status: stable
tags: [guide, handoffs, terraform, ansible, convergence]
---

# Provision-to-converge handoffs

Infrastructure in a modern homelab often requires two phases: provisioning virtual machines or network topology with OpenTofu, followed immediately by host configuration with Ansible.

exocomp unites these into a single **ledger handoff workflow** (`exocomp handoff run` or `exocomp provision run`).

## The provision → onboard → converge chain

The provision workflow executes four sequenced steps under one job ID:

```
[1. tofu apply] ──► [2. await SSH] ──► [3. host onboard] ──► [4. ansible playbook]
```

1. **`tofu apply`**:
   Applies the workspace plan to create the virtual machine or instance. Declared non-sensitive outputs (such as IP addresses and hostnames) are captured automatically.
2. **`await SSH`**:
   Polls the newly created IP address until the SSH port is reachable, bounded by a configurable timeout.
3. **`host onboard`**:
   Connects using the fleet identity key, validates the initial host key, and registers the fingerprint into `known_hosts`.
4. **`ansible playbook`**:
   Executes the configuration playbook targeting the synthetic inventory `tf:<workspace>`.

### Executing a handoff

Run the chained workflow from the CLI:

```bash
exocomp provision run prod playbooks/node.yml
```

Inspect the step ledger and current workflow state with:

```bash
exocomp provision status prod
```

If a daemon restart or crash interrupts execution mid-chain, the sequencer automatically resumes the in-flight workflow from the SQLite step ledger upon reboot without re-applying completed infrastructure steps (Decision 0008). Interrupted standalone jobs can be inspected and re-queued via `exocomp jobs redrive`.

## The dynamic `tf:<workspace>` inventory

You never need to copy-paste generated IP addresses from OpenTofu state into Ansible inventory files.

When a workspace outputs an inventory map:

```hcl
output "hosts" {
  value = {
    "k3s-master" = {
      ansible_host = hcloud_server.master.ipv4_address
      ansible_user = "debian"
    }
  }
}
```

Ansible playbooks run against `--limit tf:prod` or reference `hosts: tf:prod` directly.

## The reverse pattern: Ansible-first bootstrap

For environments where bare-metal nodes or hypervisors must be prepared before OpenTofu can communicate with them (such as bootstrapping a Talos or K3s cluster):

```bash
exocomp bootstrap run playbooks/cluster-init.yml cluster-config
```

Phase 1 runs the Ansible play to stand up the control plane. Sensitive tokens (such as `kubeconfig`) are captured directly into `/dev/shm` in RAM, encrypted into the secrets envelope, and passed to the subsequent OpenTofu workspace without touching persistent disk.
