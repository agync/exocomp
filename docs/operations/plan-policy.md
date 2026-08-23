# Gating tofu plans with policy

A plan policy is a CEL rule over a workspace's `tofu show -json` plan output. It
is evaluated the moment a plan is sealed, and its verdict travels with the
sealed plan: a `deny` makes that plan unconsumable, so a later apply of it
refuses; a `require_approval` routes that apply through the same approval gate a
destructive rule uses. The rule that decides is named in every message, so an
operator reads "why did this refuse" straight off the output.

## Writing a policy

```
exocomp policy add no-deletes \
  --workspace homelab \
  --action deny \
  --expr 'plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))'
```

The single variable is `plan`, bound to the parsed plan-show JSON — the same
document `tofu show -json <planfile>` prints. The keys a policy commonly reaches
for are `resource_changes[]` (pending config changes) and `resource_drift[]`
(what moved under tofu), each carrying `.change.actions[]` (`create`, `update`,
`delete`, `no-op`, …). The expression must evaluate to a bool.

The expression is compiled against the engine's own environment at `add` time,
so a rule that will not compile — a typo, or one that yields something other
than a bool — is refused here rather than silently failing a seal later. A
policy that cannot decide is never stored: it can never quietly allow a plan.

Two actions:

- `deny` — a matching plan is sealed **unconsumable**. `workspace apply --plan`
  of it refuses, naming the policy, and enqueues nothing.
- `require_approval` — a matching plan's apply is **gated**: the job lands in
  `awaiting_approval` and an approval is created. Release it the usual way
  (`exocomp approval link <id>`, then the decision link).

List and remove:

```
exocomp policy list
exocomp policy rm no-deletes
```

## What the operator sees

At plan time, a matched policy prints its verdict and records it on the plan's
job timeline:

```
saved plan artifact 12 at .../homelab-….tfplan.age
plan policy: deny by "no-deletes"
the sealed plan is unconsumable; a later apply of it will refuse
```

At apply time:

```
$ exocomp workspace apply homelab --plan 12
Error: workspace apply: plan 12 is unconsumable: denied by policy "no-deletes"
```

or, for `require_approval`:

```
plan 12 requires approval by policy "gate-changes"
job 34 gated (awaiting_approval); approval 5 expires 2026-08-22 14:30:00Z
print its decision link with `exocomp approval link 5`
```

## Boundaries worth knowing

- The verdict record carries only the policy name and action — never a plan
  attribute value — so it is safe in the job timeline and in any export. The
  plan itself stays encrypted at rest; policy evaluation reads it only in
  memory, for the length of the seal.
- Evaluation re-describes the sealed plan with `tofu show -json`, installing the
  providers offline from the warm plugin cache into a throwaway data dir. No
  state backend, bearer, or passphrase is involved, and the workspace's
  encrypted state is never read.
- If a workspace has policies but the plan cannot be described or evaluated, the
  plan is sealed **denied** (fail-closed): a plan no policy could examine is not
  allowed to apply unseen.
- A workspace with no policies pays nothing — the plan is never even described.
- On a workspace **with enabled policies**, a **fresh** apply (no `--plan`) is
  refused: there is no plan for a policy to read, so allowing one would be a
  bypass of the gate you installed (finding AI-4). Plan, then apply that plan by
  id.
- On a workspace **with enabled policies**, an apply of any plan that carries no
  recorded verdict is refused — an operator's own plan **file**
  (`--plan /path/to.tfplan`), or a sealed plan whose verdict has not landed yet.
  "No verdict" is read as *not adjudicated*, never as *unpoliced*, so an apply
  issued in the instant between the artifact row and the verdict record cannot
  slip past a deny (finding AI-3, `docs/security/threat-model.md`). Re-plan and
  apply that plan by id. On a workspace with no policies, a plan file applies as
  before.
