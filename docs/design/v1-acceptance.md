---
type: Design
title: "v1.0 acceptance bar"
description: "The five end-to-end stories that gate v1.0."
status: stable
tags: [design, acceptance]
---

# v1.0 acceptance bar — core functionality must be reliable first

v1.0 is not "the milestones shipped"; it is "the acceptance scenarios below work
reliably end-to-end." The post-v1 program (Litestream, GoAlert, OpenBao) does not
begin until this bar is met. Reliability of the core comes before breadth of
features. Each scenario runs end-to-end on real infrastructure (a Lima Fedora node
for the host-facing runs, real tofu/ansible/git/restic) behind its own gate script.

## The acceptance scenarios (each must run end-to-end, gate-tested)

1. **"Here is my git repo, go run my playbook regularly."** Register a git
   source, deploy key, auto-sync a signed ref, schedule the play; a bad commit
   cannot reach the fleet (signed-tag pin advanced only by a gated run).
2. **"On event/alert, run X."** Alert → CEL rule → guardrails → (optional gate)
   → run. (Already built; must stay reliable.)
3. **"Run my terraform regularly and notify me of drift."** Scheduled
   `plan`/`-refresh-only`, drift surfaced with resource-level detail, notified —
   true drift distinguished from pending config change.
4. **"Load my working directory into the server and run ansible/terraform."**
   Host-path and in-container-path sources (Decision 0010) run the same as a repo,
   minus sync.
5. **"Run terraform, then trigger dynamic inventory FROM terraform for my
   ansible play and execute it."** The IaC→CM handoff, native: a workspace's
   encrypted tfstate is an inventory source for a play, in one process, without
   leaking state secrets. This is the scenario exocomp can do that a hand-rolled
   setup cannot — and the one that most justifies the whole tool.

## What gates the bar

- The convergent credential-plane must-fix: sealed secrets bound to a run as named
  env, so dynamic inventories and providers authenticate under the sandbox. None
  of the acceptance scenarios work without it.
- The source-model work covers the git-source and host/in-container-path
  scenarios, plus the ansible/tofu run fidelity.
- The tofu-plan schedule kind + drift detail + notify (true-drift vs
  pending-config split from `-refresh-only`) covers the drift-notification
  scenario.
- The Terraform→Ansible inventory handoff is a new native capability (see below).
- Every scenario ships with an e2e gate script exercised on the Lima node, run
  from a fresh clone in maintenance.

## The Terraform→Ansible inventory handoff

exocomp uniquely holds the encrypted tfstate AND runs the ansible. The native design:

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
is a step-ledger workflow (Decision 0008); an apply→play dependency edge is the minimum.

## Sequencing

Source model + credential plane + surfaces → upgrade/rollback + plan policy + step
ledger + the Terraform→Ansible inventory handoff → the v1.0 gate proving the full v1
scope + maintenance → **declare v1.0** → THEN post-v1 (Litestream/GoAlert/OpenBao).
