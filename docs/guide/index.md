# Operator user guide

This guide walks through everyday homelab management with exocomp: managing Git sources, running Ansible playbooks, provisioning OpenTofu workspaces, chaining handoffs, binding sealed secrets, configuring reachability checks, and reacting to alerts.

All execution passes through the single audited job queue and runs under the `nono` sandbox tier.

## Getting started

| Guide | Covers |
|---|---|
| [Quickstart](quickstart.md) | 15-minute quickstart: image deploy via Quadlet, first-run wizard, and printing the printed recovery card |
| [Sources and inventory](sources.md) | Registering Git repositories, local directories, SSH fleet identities, and auto-onboarding hosts |

## Core workflows

| Guide | Covers |
|---|---|
| [Ansible automation](ansible.md) | Running playbooks, ad-hoc commands, host limits, extra vars, tags, and `--check` drift detection |
| [OpenTofu infrastructure](opentofu.md) | Managing workspaces, SQLite HTTP state backend, plan reviews, CEL plan policies, and state version history |
| [Provision-to-converge handoffs](handoffs.md) | Chaining OpenTofu apply to Ansible playbook execution in a single ledger workflow |
| [Secrets and fleet keys](secrets.md) | Age envelope encryption, SOPS integration, host identity keys, and sealed run-secret bindings |
| [Health checks and probes](checks.md) | Declaring target probes, CIDR network groups, reachability matrix, and check-triggered runs |
| [Reactive automation](reactive.md) | Webhooks, CEL event rule routing, push alerts, silence windows, and phone approval gates |

## Surfaces and companions

| Guide | Covers |
|---|---|
| [Mobile companion](mobile.md) | QR code pairing, ntfy push notifications, and one-thumb approval decisions on phone |
| [AI agent integration](agent-mcp.md) | Connecting coding agents (Claude, Cursor, Antigravity) to the `/mcp` endpoint with scoped bearer tokens |
