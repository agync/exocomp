---
type: Guide
title: "Sealed tofu outputs as run-secrets"
description: "A sensitive output sealed into the envelope and bound to an env name, so the play receives it without anything copied by hand."
status: stable
tags: [operations, terraform, secrets]
---

# Sealed tofu outputs as run-secrets

A `sensitive = true` output never reaches the materialized inventory or an
event. It is not dropped either: the apply seals it into the envelope and binds
it to an environment variable, and the workspace's own dependent play receives
it as that variable. This is the managed-DB case end to end — tofu generates the
password, the configure play seeds the database with it — with nothing copied by
an operator and no value on an argv.

It is the secrets half of the [Terraform→Ansible
handoff](terraform-ansible-handoff.md): the non-sensitive outputs become the
play's inventory, the sensitive ones become its environment.

## What an apply does with a sensitive output

```hcl
output "db_password" {
  value     = random_password.db.result
  sensitive = true
}
```

Three deterministic names, so the producer and the consumer agree on them
without a round-trip:

| Thing | Name |
|---|---|
| Sealed secret row | `tf-output/<workspace>/<output>`, kind `tofu-output` |
| Run-secret binding | `tf:<workspace>` — the same name as the standing inventory |
| Environment variable | the output name upper-cased: `db_password` → `DB_PASSWORD` |

Both halves are idempotent. A re-apply reseals the same row (the value rotates
in place, so the next play resolves the latest apply's value) and finds its
binding pair already there; nothing duplicates.

## Automatic, and scoped by the inventory the run names

There is no flag. A run inherits a workspace's sealed outputs when it targets
that workspace's standing `tf:<workspace>` inventory, and only then:

| Run | Inherits |
|---|---|
| `provision run` step 4, `handoff run` step 2 | yes — both target `tf:<workspace>` |
| `playbook run <play> --inventory tf:<workspace>` | yes |
| `schedule add … --kind play --target tf:<workspace>` | yes, on every fire |
| a run naming another inventory, a plain path, or none | no |
| the workspace's own apply | no — a tofu run names no inventory |

**Why automatic rather than opt-in.** The inventory name is already what every
dependent step of the chain carries, and it is already the thing that scopes a
run to one workspace. A flag would have to be repeated on the chain, on the
recurring converge, and on each ad-hoc run — and forgetting it is exactly the
"operator copies the password by hand" failure this closes. Making the target
the opt-in keeps the blast radius where it belongs: a run that has nothing to do
with a workspace cannot inherit its secrets, because it does not name its
inventory.

Three sources fill a run's environment, lowest precedence first: the binding a
`tf:` target implies, then `--run-secrets`, then a dynamic inventory's
`env_binding`. An explicit binding therefore overrides one an apply produced.

## When an output is sealed but not bound

The apply's capture event says so, by name and with a reason, and the run still
succeeds — the value is stored either way, only the automatic binding is
missing:

```
captured 1 output(s) to inventory /…/edge.json naming 1 host(s); sealed 2 sensitive
output(s); 1 not bound to env (db-password: not bound, an env name must be a POSIX
identifier)
```

Two cases:

- **The name cannot be an environment variable.** Terraform allows a dash in an
  output name; a POSIX identifier does not. Upper-casing is a case fold and is
  applied; substituting characters would be rewriting a name the module never
  declared, so it is refused instead. Rename the output, or bind it yourself:
  `exocomp run-secret add tf:edge DB_PASSWORD tf-output/edge/db-password`.
- **The environment name is already bound to something else** under
  `tf:<workspace>` — two outputs folding to one name, or a pair you added by
  hand. exocomp never repoints an existing binding at a different secret.

## Values-free everywhere

Only names travel. The job row records the binding name, the binding records
`(env name -> secret name)`, and the capture event records output names. The
value stays sealed in the store, is opened through the envelope for one run,
reaches the child process as an environment variable rather than an argument,
and is added to that run's redactor so it cannot survive in an event even if a
play prints it.

```
exocomp run-secret list tf:edge      # DB_PASSWORD -> tf-output/edge/db_password
exocomp run-secret rm tf:edge        # drop every pair the workspace bound
```

## Limits worth knowing

- **A removed or renamed output leaves its pair behind.** The binding is
  updated per apply, not reconciled against the module, so an output you delete
  keeps its `(env name -> secret name)` pair and its sealed row. Clear the
  binding with `run-secret rm` and re-apply if that matters.
- **A pair whose secret row was deleted fails the run.** Resolution is
  fail-closed: a play would otherwise start with a credential it thinks it has.
