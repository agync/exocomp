# Blue phase — kill the exploit, test-first

Blue closes each `exploited` finding by making the red-team repro a regression
test that fails first and passes after the fix. The sequence is not negotiable:
a fix without a first-failing test cannot prove it addressed the attack.

## Lane discipline

- Work in a worktree under `.worktrees/`, one branch per wave, all git via
  `git -C .worktrees/<lane>`. Never edit the repo root or another lane's files.
- Declare a file-ownership map up front. A finding's fix that needs a file
  another lane owns (the store seam, the queue, `internal/health`) does not
  reach across — it closes what this lane owns and names the rest as a residual
  with the owning file (see the `AI-*-resid` entries in the threat model for the
  pattern).
- `main` stays linear: rebase then fast-forward. Merge commits are for milestone
  integration only.

## The sequence, per finding

1. **Reproduce as a failing test.** Write the exploit as a Go test against the
   real handler or service verb. Run it and watch it FAIL — that failure is the
   proof the attack works. A test that passes before the fix tested nothing.
2. **Fix at the lowest face that covers every caller.** Prefer the service verb
   over per-face patches so `/api/v1`, the CLI, and MCP all inherit the guard —
   e.g. plan-policy is refused in the service so every face is covered, not once
   per handler. Land the default **fail-closed**: absence of a verdict, an
   unknown scope, an unconfirmable owner all refuse.
3. **Prove the exploit dead.** The SAME test now passes. Do not rewrite the test
   to suit the fix; the attack it encodes must be unchanged.
4. **Mutation-test the guard** (next reference) before the finding is called
   closed.

## What the fix must not do

- Do not resolve ambiguity toward "allow". "No policy record" is unconsumable,
  not unpoliced.
- Do not key a safety check on a value an attacker supplies where a
  server-derived one exists. Derive the capability, the actor, the plan owner
  server-side.
- Do not narrow the test to the one payload you fixed; cover the class (a
  fresh apply AND a plan-then-apply-by-id, an operator token AND a self-approval,
  a relative escape AND an absolute path).

## Removing a mitigation

Not every blue-team change adds a guard — some remove one (a redundant check,
a mitigation superseded by a better one). There is no new exploit to write for
a deletion, so the per-finding sequence above does not apply as-is:

1. Prove the RETAINED guards still bound the threat the deleted mitigation
   covered — the existing regression test(s) for that threat must still pass
   unmodified against the code with the mitigation removed.
2. Mutation-test the RETAINED guard, not the deleted one — revert it and
   confirm its test still goes red. A deletion that leaves the retained
   guard's own mutation check green means the retained guard was never
   carrying the weight the deletion assumed it was.
3. Record the threat-model row as a disposition **CHANGE**: the finding is
   neither newly FIXed nor freshly HARDENed, its guard shape changed. Name the
   residual risk the deletion leaves (if any) and which file owns it, same as
   any other disposition.

## Commit and gate

- Logical-unit commits, one finding per unit where they are separable.
  Conventional title `<= 52` chars, terse body saying what the change does, NO
  trailers, explicit path staging (never `git add -A`).
- The pre-commit hook runs the full `make check` (`fmt-check vet lint tidy-check
  test`) via `.agents/hooks/`; activate with `git config core.hooksPath
  .agents/hooks` once per worktree. Paste the real green output before claiming
  done — verification precedes the claim.
- Two tests are load-flaky under the full run:
  `TestTwoProcessesSerializeAConcurrencyKey` (`cmd/exocomp/concurrency_test.go`)
  and `TestServeShutdownRetiresWhatItCannotFinish`
  (`cmd/exocomp/serve_integration_test.go`). If one fails, re-run before
  assuming a regression — they are timing-sensitive, not broken by your change.
