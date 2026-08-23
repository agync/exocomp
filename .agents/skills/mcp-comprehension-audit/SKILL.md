---
name: mcp-comprehension-audit
description: Evaluate how well exocomp's MCP tool catalog explains itself to a context-free agent, so tool calls are correct and efficient. Use when the MCP surface changes — a tool, description, input schema, resource, or scope-visibility edit — to score naive-reader comprehension, propose rewrites, and prove them by re-test. Pairs with the mcp-comprehension-auditor agent.
---

# MCP comprehension audit

A description passes not when it reads well to someone who built the tool, but
when an agent that has never seen exocomp calls the tool correctly from the text
alone. This skill measures that gap and closes it.

Runnable by the `mcp-comprehension-auditor` agent or inline. It is a *text*
audit — the Go tests already prove behaviour; here you prove the words match the
behaviour and are cheap to carry.

## Preconditions

- The MCP catalog is **frozen** for the run. Never audit a branch still editing
  `internal/mcp/tools.go` — the readers would score text about to change.
- You can obtain the live catalog per scope (preferred) or read it from source.

## The five steps

Read each reference only when you reach its step — that is the whole point of
keeping this file short.

1. **Extract the catalog and establish ground truth.** Pull `tools/list` and
   `resources/list` for each scope (`readonly`, `operator`, `approve`); record,
   privately, what each tool truly does. → `references/extract-and-iterate.md` §1
2. **Run the naive readers.** Spawn `haiku` subagents — one per tool (isolated
   comprehension) plus one catalog reader per scope (disambiguation) — each fed
   only client-visible text and the fixed questionnaire.
   → `references/naive-reader-protocol.md`
3. **Score against ground truth** on the rubric: intent, when-to-call, argument
   construction, return expectation, side-effect/danger awareness, then brevity
   as the tie-break. → `references/scoring-rubric.md`
4. **Rank and rewrite.** Surface weak and — above all — dangerous-misread tools;
   write a concrete before/after for each. → `references/extract-and-iterate.md` §2
5. **Re-audit and report.** Fresh naive readers score the rewrites; keep a
   rewrite only if its delta is positive. Report scores, quoted misreads,
   rewrites, deltas, and disambiguation results.
   → `references/extract-and-iterate.md` §3

## Non-negotiables

- No exocomp context — not the product name, not the repo, not this skill — ever
  reaches a naive-reader prompt. A leaked hint invalidates the run.
- The auditor proposes; landing a rewrite into `internal/mcp/tools.go` is a
  separate gated lane.
- A rewrite is earned by a re-test delta, not by taste.
