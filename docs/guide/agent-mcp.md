---
type: Guide
title: "AI agent integration"
description: "Connecting AI coding assistants (Claude Code, Cursor, Antigravity) via Model Context Protocol (MCP)."
status: stable
tags: [guide, mcp, agents, automation, llm]
---

# AI agent integration

exocomp serves an embedded **Model Context Protocol (MCP)** server over HTTP at `POST /mcp` on its LAN listener (Decision 0009 and Decision 0014).

This allows developer assistants (such as Claude Code, Cursor, Antigravity, and Cline) to query homelab state, inspect job outputs, review OpenTofu plans, and queue operational playbooks under strict governance.

## Generating an agent token

Mint a scoped bearer token for your agent:

```bash
exocomp token new agent-readonly --scope readonly

# Or with operator write permissions:
exocomp token new agent-operator --scope operator
```

Tokens respect three granular scopes (Decision 0009):
- `readonly`: Read-only queries over jobs, workspaces, drift, checks, netgroups, approvals, sources, schedules, rules, and events.
- `operator`: All read tools, plus `workspace_plan`, `workspace_apply`, and check/netgroup management.
- `approve`: All read tools, plus `approve_job` and `reject_job`.

## Connecting an agent

### Claude Code / Antigravity / Cursor MCP configuration

Add exocomp to your MCP client configuration (e.g. `claude_desktop_config.json` or `.gemini/settings.json`):

```json
{
  "mcpServers": {
    "exocomp": {
      "url": "https://<host>:<port>/mcp",
      "headers": {
        "Authorization": "Bearer exo_..."
      }
    }
  }
}
```

## Available agent tools

The MCP surface exposes structured tools designed for machine comprehension (see [The MCP surface](../operations/mcp.md) for full catalog and schema):

| Tool | Purpose |
|---|---|
| `list_jobs`, `get_job` | List jobs and inspect job state and exit codes |
| `get_job_events` | Read execution append-only, redacted event log stream |
| `list_workspaces`, `get_workspace` | Inspect configured OpenTofu workspaces |
| `get_drift` | Inspect cached workspace drift and pending plan changes |
| `workspace_plan` | Enqueue an OpenTofu plan for a workspace |
| `workspace_apply` | Apply a workspace or sealed plan artifact |
| `list_checks`, `get_check`, `probe_check` | Inspect probe checks or run live network probes |
| `list_approvals`, `approve_job`, `reject_job` | Inspect and settle pending human-in-the-loop approvals |

*(Note: Ansible playbook runs are initiated via `POST /api/v1/playbooks/run` or the CLI rather than MCP, ensuring inventory and path resolvers remain strictly pinned).*

## Security guarantees for agents

1. **All execution passes through the queue**: An agent cannot bypass the scheduler or execute arbitrary host shell commands.
2. **Secrets never travel across MCP**: Secret values are completely stripped and replaced with `[redacted · secret]`.
3. **Approval boundaries**: If an operation is marked as requiring approval, the MCP tool returns a status of `pending_approval` with the approval ID; the agent cannot self-approve without explicit `approve` scope.
