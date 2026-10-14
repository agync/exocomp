---
type: Design
title: "Personas"
description: "The four target users: their estate, their goals, and the constraint that decides adoption."
status: stable
tags: [design, users]
---

# Personas

exocomp targets one operator running a handful of hosts, but that operator arrives from
several backgrounds. These four personas define the target users: their estate, what they
want the tool to do, and the constraint that decides whether they adopt it. Features are
justified against these definitions, not against a generic "homelab" audience.

## The veteran homelabber

Runs a large, mature Ansible repo (100+ tasks) against a fixed set of physical hosts, plus
compose and LXC workloads. Has outgrown a cron box and a hand-written Makefile and has left
AWX behind as too heavy.

- **Wants:** scheduled playbook runs with history, a plan-equivalent before changes
  (`--check --diff` with a changed-count as a drift signal), ad-hoc runs, and structured
  (JSON) task output rather than line-scraped logs.
- **Values:** no bespoke formats — state that any standard tool can read back is worth more
  than any feature AWX offered.
- **Adoption constraint:** inventory must be a real, designed surface, and vault passwords
  must be derivable from the printed recovery card, so a dead daemon never bricks the
  operator's own playbooks.

## The staff SRE

Applies professional infrastructure reflexes (Terraform plan/apply discipline, secret
management) to a home estate, but refuses to run enterprise-weight operations tooling
outside work. Cares most about the approval boundary and about not being paged by routine
maintenance.

- **Wants:** a saved-plan / gated-apply model, policy evaluated over the plan JSON before
  the approval gate, a structured diff on the phone with sensitive values suppressed, and
  git provenance that refuses to apply if the tree moved under the plan.
- **Values:** maintenance windows and silences with a TTL — reboots and planned work must
  not page or trigger runbooks.
- **Adoption constraint:** silences and a rehearsed key-generation restore drill are hard
  blockers; plan artifacts and approval rendering are the least-protected place a secret
  appears and must be treated as such.

## The time-poor generalist

Comfortable with bash and compose but not with Ansible or OpenTofu. Wants backups that are
verifiably green and a recovery card in the fire safe, with everything else out of the way
until needed.

- **Wants:** a wizard with a recommended default (not a TPM/YubiKey decision on screen one),
  shell runbooks as a first-class on-ramp, compose awareness, and a NAS-only setup that is a
  legitimate finish rather than a half-configured state.
- **Values:** the dead-man's switch — a monitoring tool whose failure mode is a loud silence.
- **Adoption constraint:** week one must be "wizard → backups green → card printed" with no
  other decisions forced.

## The security engineer

Runs SELinux enforcing, reads decision records and licenses before installing, and assumes any node can
be lost or seized. Trusts the tool only where its claims are demonstrated, not asserted.

- **Wants:** signed releases with an SBOM and reproducible builds, an in-repo signed
  installer whose hash is on the recovery card, a versioned keyring and a MAC over the backup
  manifest that refuses downgrades, and a threat model shipped with the release.
- **Values:** the CI-executed recovery card, assert-don't-assume health probes, the
  exit-code trust boundary, and honesty about what the envelope does and does not protect.
- **Adoption constraint:** a written no-egress commitment and integrity guarantees against
  keyring rollback — silent unrecoverability is the one failure this persona will not accept.
