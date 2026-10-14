---
type: Guide
title: "OpenTofu infrastructure"
description: "Managing infrastructure with OpenTofu: SQLite HTTP state backend, plan diffs, CEL policies, and sealed outputs."
status: stable
tags: [guide, opentofu, terraform, state, drift]
---

# OpenTofu infrastructure

exocomp provides native lifecycle management for OpenTofu (`opentofu`) configurations, combining an embedded HTTP state backend with granular plan-diff rendering and safety guardrails.

## The SQLite HTTP state backend

exocomp implements OpenTofu's HTTP state backend directly inside `exocompd` backed by the local SQLite database (Decision 0004):

- **No external S3 or MinIO required**: Eliminates the conditional-write/consistency bugs of self-hosted object stores.
- **Automatic version history**: Every state update is recorded with full rollback history.
- **State locking**: Prevents concurrent modifications across jobs.

Configure the backend in your OpenTofu configuration (or run `exocomp workspace add <name> --state-only` to generate the exact ready-to-paste block and bearer token):

```hcl
terraform {
  backend "http" {
    address        = "http://127.0.0.1:8543/api/v1/tf/state/prod"
    lock_address   = "http://127.0.0.1:8543/api/v1/tf/state/prod"
    unlock_address = "http://127.0.0.1:8543/api/v1/tf/state/prod"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
```

## Running plans and applies

1. **Register the workspace and trigger a plan**:
   ```bash
   exocomp workspace add prod --dir terraform/dns
   exocomp workspace plan prod
   ```
   The plan is executed inside the sandbox, parsed, classified, and sealed into SQLite as an immutable plan artifact.

2. **Inspect the plan diff**:
   In the Web UI, the workspace view renders high-contrast, structured plan diffs:

   ![OpenTofu plan diff viewer](../design/assets/tofu/1b-plan-diff.png)

   The diff engine implements strict formatting invariants:
   - Top-level non-server-set keys are counted precisely (`N of K keys changed`).
   - Server-set fields (such as `uid` and `resource_version`) are filtered.
   - Sensitive values are automatically redacted (`[redacted · secret]`).

3. **Apply the plan**:
   ```bash
   exocomp workspace apply prod
   ```
   If configured with an approval policy or triggered via webhook, the apply pauses at an approval gate until confirmed via Web UI, TUI, or mobile push notification.

## Gating plans with CEL policies

You can prevent destructive actions (e.g. dropping a production database or destroying more than 5 resources) using Common Expression Language (CEL) policies:

```yaml
# .exocomp/policies/safe-apply.yml
name: max-destructions
rule: "plan.resource_changes.filter(r, 'delete' in r.change.actions).size() < 5"
action: deny
message: "Refusing plan: destroys more than 4 resources"
```

If a plan violates a CEL policy, exocomp seals the plan with a status of `refused` and rejects any attempt to apply it.

## Binding sealed outputs

When an OpenTofu workspace exports a sensitive output (such as an admin token or generated database password), mark it as sensitive:

```hcl
output "db_password" {
  value     = random_password.db.result
  sensitive = true
}
```

exocomp encrypts this value directly into the age secrets envelope as a sealed run-secret. Subsequent Ansible playbooks targeting the workspace can consume the output as an environment variable (`$DB_PASSWORD`) without exposing secrets in logs or state files.
