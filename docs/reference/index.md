---
type: Hub
title: "Technical reference"
description: "Reference specifications for the CLI, configuration files, REST API, MCP endpoints, and error taxonomy."
status: stable
tags: [reference, cli, api, configuration, specs]
---

# Technical reference

Precise, contract-level specifications for exocomp command-line tools, configuration file formats, wire protocols, and machine-readable output envelopes.

## Reference documents

| Reference | Description |
|---|---|
| [Command line interface (CLI)](cli.md) | Syntax, commands, global flags, and JSON envelope output (`--output json`) |
| [Configuration files](configuration.md) | Formats and schemas for `.exocomp/hooks.yml`, Quadlet unit definitions, and policies |
| [REST API (`/api/v1`)](../operations/api.md) | Wire endpoints, authentication, idempotency headers, and HTTP status mappings |
| [MCP tool catalog](../operations/mcp.md) | Model Context Protocol tools, input schemas, and scope definitions |
| [Exit codes and error taxonomy](exit-codes.md) | Exit code mappings, error domains, and failure classifications |
