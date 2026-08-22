# v1.0 acceptance bar — core functionality must be reliable first

Operator directive (2026-08-22): v1.0 is not "the milestones shipped"; it is
"these end-to-end stories work reliably." The post-v1 program
([post-v1-plan.md](../../.agents/workflows/post-v1-plan.md): Litestream, GoAlert,
OpenBao) does NOT begin until this bar is met. Reliability of the core over
breadth of features.

## RESULT — v1.0 MET (2026-08-22)

**All five stories pass end-to-end. `test/e2e/v1-acceptance-gate.sh`: 6 passed, 0
skipped, 0 failed.** Each story has its own gate, exercised on real infra (a Lima
Fedora node for the host-facing runs, real tofu/ansible/git/restic throughout):

| Story | Gate | Verdict |
|---|---|---|
| 1 · git repo → run playbook regularly (signed ref) | `test/e2e/m3d-story1-gate.sh` | PASS |
| 2 · on event/alert → run X | `test/e2e/m3d-story2-gate.sh` | PASS |
| 3 · run terraform regularly, notify drift | `test/e2e/m3d-story3-gate.sh` | PASS |
| 4 · load working dir → run ansible/terraform | `test/e2e/m1-gate.sh` (Lima) + `m2-gate.sh` | PASS |
| 5 · terraform → dynamic inventory FROM terraform → play | `test/e2e/m3c-story5-gate.sh` | PASS |

The four M3c capability gates (`m3c-{policy,upgrade,lifecycle}-gate.sh`) and the
backup/DR + daemon coverage in m1/m2 also pass. Story 1's trust anchor gap found
during M3d — trusted signers came from ambient git config, not exocomp — was fixed
(exocomp-owned `--allowed-signers`, migration 0014). **v1.0 is declared; the post-v1
program may begin.**

## The acceptance stories (each must run end-to-end, gate-tested)

1. **"Here is my git repo, go run my playbook regularly."** Register a git
   source, deploy key, auto-sync a signed ref, schedule the play; a bad commit
   cannot reach the fleet (signed-tag pin advanced only by a gated run).
2. **"On event/alert, run X."** Alert → CEL rule → guardrails → (optional gate)
   → run. (M3a — built; must stay reliable through M3b.)
3. **"Run my terraform regularly and notify me of drift."** Scheduled
   `plan`/`-refresh-only`, drift surfaced with resource-level detail, notified —
   true drift distinguished from pending config change.
4. **"Load my working directory into the server and run ansible/terraform."**
   Host-path and in-container-path sources (ADR 0010) run the same as a repo,
   minus sync.
5. **"Run terraform, then trigger dynamic inventory FROM terraform for my
   ansible play and execute it."** The IaC→CM handoff, native: a workspace's
   encrypted tfstate is an inventory source for a play, in one process, without
   leaking state secrets. This is the story exocomp can do that a hand-rolled
   setup cannot — and the one that most justifies the whole tool.

## What gates the bar

- The convergent credential-plane must-fix (expert-reviews.md): sealed secrets
  bound to a run as named env, so dynamic inventories and providers authenticate
  under the sandbox. Nothing in stories 1–5 works without it.
- The M3b sources lane owns 1, 4, and the ansible/tofu run fidelity.
- Story 3 = M3a's tofu-plan schedule kind + drift detail + notify (fold the
  -refresh-only true-drift split forward from v1.1 if needed for reliability).
- Story 5 = a new native capability (see below), designed from the experts'
  "how do you do it today" answers.
- Every story ships with an e2e gate script (like m1/m2-gate.sh) exercised on
  the lima node, run from a fresh clone in maintenance.

## Story 5 — the Terraform→Ansible inventory handoff (design pending experts)

exocomp uniquely holds the encrypted tfstate AND runs the ansible. The native
design (finalised from both experts, who disagreed on a security-critical point):

**Governing rule — never expose raw state.** A tfstate embeds every resource
attribute in the clear, including provider secrets and generated passwords. So
the inventory is built from **declared `output` blocks, not the state file**. At
apply end, exocomp runs `tofu output -json` *inside the same sandboxed run that
already holds the bearer and the pbkdf2 passphrase*, and persists **only the
declared, non-sensitive outputs** as a workspace inventory artifact. A dependent
Ansible job consumes that artifact as `--inventory` from its own 0700 private
dir. The fleet-facing run never receives a bearer, never reads a `tfstate` row,
and sensitive outputs are redacted from logs by the existing redactor. This is
the capability that justifies one tool holding both halves — a hand-rolled
`terraform output | jq` cannot make the boundary safe, because it must hand the
state key to the consumer.

- **Primary (v1):** explicit outputs → `tofu output -json` capture → inventory
  artifact → chained play. The operator shapes hosts/groups via outputs (or a
  documented convention).
- **Ergonomic (opt-in, v1.1):** the `cloud.terraform.terraform_provider`
  inventory plugin reading the loopback backend — auto-maps resources→hosts, but
  sees full state, so it is opt-in with a stated warning and strips
  sensitive-marked attributes.

The chain — apply succeeds → outputs captured → inventory refreshes → play runs —
is a step-ledger workflow (ADR 0008); an apply→play dependency edge is the
minimum. Materialization and capture are neither built today.

## Sequencing

M3b (source model + credential plane + surfaces) → M3c (upgrade/rollback + plan
policy + step ledger + story-5 handoff) → M3d (v1.0 gate proving all five stories
+ maintenance) → **declare v1.0** → THEN post-v1 (Litestream/GoAlert/OpenBao).
