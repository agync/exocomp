# Extract, iterate, report

The mechanical steps around the reader protocol and rubric.

## §1 Extract the catalog and establish ground truth

Get the catalog as an MCP client sees it, per scope:

- **Live (preferred):** speak JSON-RPC to the server. Over stdio,
  `exocomp mcp` then `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` and
  `resources/list`; over HTTP, the same methods against the MCP endpoint with a
  bearer token of the scope under test. Capture the exact `name`, `description`,
  `inputSchema` for each scope — the scope-filtered list is what a real agent of
  that scope receives.
- **From source (fallback):** the tool catalog in `internal/mcp/tools.go` and
  the scope filter alongside it. Use only if a server is not runnable; note in
  the report that text was read from source, not the wire.

Then, privately, write the ground truth for each tool — what it does, its side
effect (read / enqueues a job / gated apply / approval decision), required args
and their meaning, and the situations an agent should call it in. Draw this from
the tool's handler, `docs/design/agent-interface.md`, and `docs/operations/`.
**This never enters a reader prompt.** It is the answer key.

## §2 Rank and rewrite

From the scored readers, list Dangerous tools first, then Weak, then merely
unclear (per the rubric severity). For each tool to rewrite, produce a concrete
before/after:

- Fix the specific misread the readers showed — do not rewrite blind. If Q3
  failed, the new text must state the side effect in words a cold reader can't
  miss ("enqueues a job; it does not run until approved" beats "applies the
  plan"). If a look-alike stole calls, add the distinguishing clause to both.
- Keep it short. Every token ships in every agent's context; aim for the
  shortest text that carries intent + side-effect + the one disambiguating fact.
- Fix the `inputSchema` too when arguments misfired: a clearer `description` on a
  property, a required-vs-optional correction, an example, an enum instead of a
  free string.

Rewrites are proposals. Landing them edits `internal/mcp/tools.go` and is a
gated implementer lane, not this agent's commit.

## §3 Re-audit and report

- Spawn **fresh** naive readers on the rewritten text (same questionnaire, same
  rubric). A rewrite is kept only if its comprehension delta is >= 0 and it
  clears the misread that motivated it; a rewrite that trades one misread for
  another is rejected.
- Report, terse:
  - per-scope table: tools Solid / Weak / Dangerous, disambiguation accuracy;
  - per weak/dangerous tool: the quoted misread, the before/after text, and the
    before/after scores;
  - anything a rewrite could not fix in text alone (e.g. two tools that are
    genuinely redundant and should be merged, or a missing tool a reader kept
    wishing for) — these are design findings for the interface, not wording.

## Reusing across runs

The audit is meant to be re-run whenever the surface changes. Keep each run's
scored table so a later run can show movement. When a run's rewrites land, the
next run's baseline is the improved catalog — the score should ratchet up, and a
regression (a new tool dragging a scope's average down) is itself a finding.
