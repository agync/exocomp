---
name: security-red-blue
description: Test-first red/blue-team method for exocomp's agent-facing surface (the write half of /api/v1, the remote CLI, the MCP server). Red refutes safety with an exploit reproduced against a running binary; blue turns each confirmed exploit into a failing-then-passing regression test, fixes it, and mutation-tests the guard. Use when that surface grows or when hardening a finding in docs/security/threat-model.md. Pairs with the security-red-blue agent.
---

# security-red-blue

A guard is safe not because its author says so, but because an attacker who
tries to defeat it fails, and a test dies the moment someone reverts it. This
skill runs that proof: red produces the exploit, blue kills it and proves it
stays dead.

Runnable by the `security-red-blue` agent or inline. It codifies how
`AI-1`..`AI-13` were closed (`docs/security/threat-model.md`) and applies the
same loop to new work on the write half of `/api/v1`, the remote CLI, and the
MCP server.

## Preconditions

- The surface is **frozen** for the run — no branch still editing the routes,
  tools, or flags under attack.
- You can build a real binary (`make build`) and init a throwaway data dir under
  a scratch `XDG_DATA_HOME`. Red grades exploits against that, not the source.

## The five steps

Read each reference only when you reach its step — that is why this file is
short.

1. **Build and freeze.** Build the binary, init a scratch data dir, mint the
   tokens/scopes the attack needs. Establish ground truth for each face from
   source and docs; the attacker's belief about what a guard does never
   substitutes for it. → `references/red-phase.md` §1
2. **RED — refute.** Enumerate concrete exploits against authority, the approval
   gate, plan-policy adjudication, idempotency/attribution, session scope, path
   confinement, and auth timing/rate. Try each. Record every finding in the
   fixed shape and rank by solo-operator risk. → `references/red-phase.md` §2
3. **BLUE — failing test first.** For each confirmed exploit, write the repro as
   a test that FAILS against current code, then fix in a worktree lane, then the
   SAME test proves the exploit dead. Defaults land fail-closed.
   → `references/blue-phase.md`
4. **Mutation-test the guard.** Patch the new guard back to the old behaviour and
   confirm the regression test fails loudly. A guard no test kills is not a
   guard. → `references/mutation-check.md`
5. **Record.** A threat-model table row per finding (`id`, `exploited`,
   disposition), the wave's commits, the mutation evidence, and honest residuals
   naming what stayed structurally open and which file owns it.
   → `references/worked-examples.md` for the shape of a well-closed finding.

## Non-negotiables

- No verdict = unconsumable. A fix that reads absence as permission is a finding,
  not a fix.
- `exploited` is reproduced against a running binary, never argued from source.
- One finding → one failing test → one fix → one mutation kill. Skipping the
  mutation step ships a guard no test defends.
- Red is read-only w.r.t. the repo; blue lands only in a `.worktrees/` lane
  behind the full `make check` gate, with logical-unit commits and no trailers.
