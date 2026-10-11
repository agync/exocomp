---
type: Guide
title: "AI agent integration"
description: "Connecting AI coding assistants (Claude Code, Cursor, Antigravity) via Model Context Protocol (MCP)."
status: stable
tags: [guide, mcp, agents, automation, llm]
---

# AI agent integration

exocomp serves an embedded **Model Context Protocol (MCP)** server over HTTP at `POST /mcp` on its LAN listener (ADR 0009 and ADR 0014).

This allows developer assistants (such as Claude Code, Cursor, Antigravity, and Cline) to query homelab state, inspect job outputs, review OpenTofu plans, and queue operational playbooks under strict governance.

## Generating an agent token

Mint a scoped bearer token for your agent:

```bash
exocomp token new --name "claude-code" --scope "read,jobs:write"
```

Tokens respect granular scopes:
- `read`: Read-only access to sources, jobs, checks, and workspaces.
- `jobs:write`: Permission to enqueue playbooks or command runs.
- `approve`: Permission to resolve pending approval gates.

## Connecting an agent

### Claude Code / Antigravity / Cursor MCP configuration

Add exocomp to your MCP client configuration (e.g. `claude_desktop_config.json` or `.gemini/settings.json`):

```json
{
  "mcpServers": {
    "exocomp": {
      "url": "http://127.0.0.1:8642/mcp",
      "headers": {
        "Authorization": "Bearer exo_tok_..."
      }
    }
  }
}
```

## Available agent tools

The MCP surface exposes structured tools designed for machine comprehension:

| Tool | Purpose |
|---|---|
| `list_jobs` | List recent jobs, execution status, and exit codes |
| `get_job_output` | Read execution stdout/stderr log stream for a job |
| `run_playbook` | Enqueue an Ansible playbook execution against a source |
| `get_workspace_plan` | Inspect structured plan diffs for an OpenTofu workspace |
| `apply_workspace` | Request execution of a sealed workspace plan |
| `list_checks` | Inspect reachability probe statuses across netgroups |

## Security guarantees for agents

1. **All execution passes through the queue**: An agent cannot bypass the scheduler or execute arbitrary host shell commands.
2. **Secrets never travel across MCP**: Secret values are completely stripped and replaced with `[redacted · secret]`.
3. **Approval boundaries**: If an operation is marked as requiring approval, the MCP tool returns a status of `pending_approval` with the approval ID; the agent cannot self-approve without explicit `approve` scope.
