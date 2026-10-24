---
type: Decision
title: "Decision 0027 — Sources, secrets and run-secrets over the API"
description: "The source, secret and run-secret registrations get /api/v1 routes under the operator scope, bearer only; a sealed value only travels in, and token minting stays off the API."
status: stable
tags: [decision, api, secrets, sources, authz]
---

# Decision 0027 — Sources, secrets and run-secrets over the API

Status: accepted · 2026-10-24

## Context

[Decision 0026](0026-remote-first-cli.md) sends every command to a configured remote daemon and
refuses the ones without a route. The source, secret and run-secret verbs had none, so a
machine that only drives a remote daemon could not register a source, seal a secret or bind one
to a run. These verbs touch the credential plane: a sealed secret, a git source's deploy key,
and the mapping that hands a sealed value to a run as env. The question is which token may do
that over HTTP, and what crosses the wire.

The browser already seals secrets, rotates and deletes them, and registers git sources, under an
operator cookie session plus a re-authentication. The [agent
interface](../design/agent-interface.md) design places `secret_add/rotate/rm` and
`source/inventory/runsecret add/rm` in the write (operator) column, with a value only ever going
in.

## Decision

**The registrations get `/api/v1` routes, and every one takes the `operator` scope as a
bearer.** `POST /sources`, `DELETE /sources/{name}`, `POST /sources/{name}/sync`,
`PUT /sources/{name}/egress`, `PUT /sources/{name}/run-secrets`, `POST /secrets`,
`PUT /secrets/{name}`, `DELETE /secrets/{name}`, `POST /run-secrets/{binding}` and
`DELETE /run-secrets/{binding}`. The binding reads, `GET /run-secrets` and
`GET /run-secrets/{binding}`, take the `readonly` floor.

- **Bearer only.** A cookie session is refused on these routes, as on every `/api/v1` write
  ([threat model](../security/threat-model.md), T24). The browser's re-authentication proves
  possession behind an ambient cookie. A bearer is that proof already, so the API adds no
  second step.
- **Operator, not a new scope.** An operator token already enqueues runs that open sealed
  values through existing bindings and `--vault-project`, and it can sign in to the browser
  and seal or rotate secrets there. A separate scope would split the same principal's authority
  across two credentials without narrowing what an operator can reach. `readonly` and `approve`
  tokens are refused before the store is read.
- **A value only travels in.** A secret's value arrives base64 in a request body over the
  pinned TLS channel, so binary bytes arrive unchanged, and is never in a response or a log
  line. Answers carry names, kinds, sources, sizes and expiry. A git source's answer carries
  the deploy key's public half once, as the CLI and the browser print it.
- **The same verbs as the CLI.** Sealing, replacing, binding and the bindings guard on delete
  are `internal/service` verbs the CLI's local path calls too. The source routes reuse the
  browser's sync, deploy-key and cache-purge helpers. The browser's own secrets form still
  seals and rotates through its handlers.
- **Secrets exocomp makes for itself stay on the host.** A git source's deploy key
  (`source/<name>/deploy-key`), the fleet ssh key (`fleet/ssh`) and a sealed play or tofu output
  (`ansible-output/...`, `tofu-output/...`) were never held by an operator, and `host onboard`
  reuses a row it finds under `fleet/ssh`. The API therefore refuses, with a `403`, to seal,
  replace, remove or bind a secret of those kinds or under those names, sealed yet or not, so a
  name cannot be bound early, squatted, or freed for exocomp to fill. It refuses as well to add
  to or remove the `ab:` and `tf:` bindings that collect outputs, or to make one a source's
  default. All of that stays possible on the daemon's host with `--local`. `host onboard` also
  refuses a row under `fleet/ssh` that it did not seal.
- **Inputs that reach git are checked.** A git source needs a git URL, and a ref may not start
  with `-` or hold whitespace, which `source.Sync` enforces for every face. A sync that git
  fails answers with a fixed message while git's output goes to the daemon log.
- **Minting stays off the API.** `token` remains local-only (Decision 0026): an operator token
  cannot mint a token of any scope over HTTP.

## Consequences

- An operator token can now bind any other sealed secret to an env name and run a play of its
  choice with it, so it can read those values. A run that names an output binding directly
  (`run_secrets: tf:<workspace>`) can read the outputs in it, on this face as on
  `/playbooks/run` before it. Through the browser it could
  already seal, replace and delete secrets, and runs already open the values in existing
  bindings. Binding a new pair over HTTP is the new reach. An operator token is therefore a
  credential for every operator-sealed value, and is handed only to a principal trusted with
  them.
- A path source names a directory on the daemon's host, writable if asked, and a run from it
  reads that tree. A sandboxed run (ansible, tofu, a governed CLI tool) is still refused the data
  dir, its ancestors and its state files (the database, the host key, the keyring), and the API
  refuses such a path when the source is added. The browser already offered the same to an
  operator session.
- **A shell schedule or rule runs unsandboxed.** The schedule and rule routes
  (`POST /schedules`, `POST /rules`) take the same `operator` scope. A `shell` schedule or rule
  runs its argv as the daemon's user through the plain shell driver, with no kernel sandbox at
  any tier, so it reaches the data dir, the keyring and the host key as any process of that user
  does. This is accepted under the single-operator trust model ([threat
  model](../security/threat-model.md), "The operator is trusted"): an operator token is trusted
  to run arbitrary commands as that user. The row records who created it (`created_by`, in the
  job ledger's words, from the API, the CLI and the browser's schedule forms), and every shell
  job writes
  `running UNSANDBOXED (shell)` to its log when it runs, from the daemon or an inline drain.
- The browser's secrets form does not reserve these names. The reservation covers the API.
- The source, secret and run-secret writes record the token in the daemon log, not on a ledger
  row, like the check and netgroup writes (threat model, "Config writes leave no actor"). A
  schedule or rule records its creator on its row.
- `source sync` runs the git fetch inside the request, as the browser's Sync does. A slow forge
  holds the request open until the sync ends or the client gives up.
