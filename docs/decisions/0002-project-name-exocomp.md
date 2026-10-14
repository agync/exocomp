---
type: Decision
title: "Decision 0002 — Project name: exocomp"
description: "Project name exocomp, after the Control D collision ruled out the working name."
status: stable
tags: [decision, naming]
---

# Decision 0002 — Project name: exocomp

## Status

Accepted · 2026-08-21

## Context

The design work was carried out under the working name **control**, with a daemon named
`controld`. A collision check before any name was published found:

- Windscribe's **Control D** DNS service ships an open-source daemon literally named
  `ctrld` — one-liner router installs, a Docker image, aimed at the same homelab audience.
  A binary named `controld` would be a standing source of confusion in exactly the forums,
  issue trackers, and search results this project's users live in.
- `control.dev` and `controld.dev` are both already registered.
- In Star Trek, "Control" is the Section 31 rogue AI — the wrong omen for a daemon whose
  thesis is refusing destructive commands.

The short alias `exo` was also checked and collides with the exo-lang compiler.

## Decision

The project is **exocomp**.

| Surface | Value |
|---|---|
| Canonical domain | `exocomp.dev` (registered) |
| Daemon | `exocompd` |
| CLI | `exocomp` |
| Installer | `get.exocomp.dev` |

The CLI is never aliased to `exo`.

The name is chosen for fit, not flavour: the exocomps of *The Quality of Life* are small
self-directed repair units that maintain station infrastructure autonomously — and
famously refuse orders that would destroy them. Autonomous maintenance with built-in
refusal of destructive commands is this design's thesis: queue-enforced guardrails,
default-deny on destructive runbook kinds, approval gates.

## Consequences

- The namespace was verified clean in the infrastructure tooling space; `exocomp.dev` and
  `exocomp.sh` were both unregistered at check time, and the former is now held.
- Every artifact name in the design carries over mechanically: `exocomp.db`, the
  `exocomp doctor` command, the `exocomp` container and volume units.
- The pre-rename documents — the research notes, the three candidate designs, and the
  critique — keep the working name "control" and are **not** rewritten. They are records
  of a process, not living specs. They are archived in the exocomp design phase of the
  lab wiki, which is the only place the old name appears.
