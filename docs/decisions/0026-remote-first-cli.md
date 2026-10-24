---
type: Decision
title: "Decision 0026 — The CLI follows its configured remote"
description: "A hosts.toml default sends every command to that daemon with no flag; --local is the one opt-out; each command is remote-capable, exempt, local-only on purpose, or pending its route, and a test fails on one that is none."
status: stable
tags: [decision, cli, remote, api]
---

# Decision 0026 — The CLI follows its configured remote

Status: accepted · 2026-10-24

## Context

The CLI is both client and server (invariant 1). Until now a command reached a remote daemon
only with `--remote` or `EXOCOMP_REMOTE`. The `hosts.toml` default that `exocomp login`
records filled only a valueless `--remote=`. A bare command opened the CLI's own data dir.

On a machine that only drives a daemon elsewhere (the daemon in a container with its own
volume, the CLI on a laptop), that data dir is a second store nothing reads. `source add`,
`secrets add`, `schedule add` and the rest wrote there and reported success, and a bare
`playbook run` ran on the laptop. A guard added before this record refused such commands when
a remote was selected, but a bare command still selected nothing.

## Decision

**A configured default remote is where every command goes.** Selection precedence:

1. `--local`: this machine's data dir. Passing it with `--remote` is an error.
2. `--remote <selector>` (an empty value means the `hosts.toml` default).
3. `EXOCOMP_REMOTE`, when set and not empty.
4. The `hosts.toml` default.
5. Nothing configured: this machine's data dir, as before.

`--local` is the one spelling of the opt-out. An empty `EXOCOMP_REMOTE` is the same as an
unset one, because a variable that changes meaning between unset and empty is easy to get
wrong in a unit file or a Makefile. A `hosts.toml` that cannot be read is an error naming
`--local`: it may hold a default, so the command neither guesses local nor remote.

**Every runnable command is in exactly one of four sets** (`cmd/exocomp/remoteguard.go`), and
`TestEveryCommandIsClassified` fails on a command that is in none or in two. The sets name full
command paths, never a parent, so a new subcommand is classified on its own:

- **Remote-capable**: it calls the API and acts on the selected daemon.
- **Exempt**: it runs on this machine by nature and ignores the selection: `version`, `help`,
  `completion`, `commands`, `login`, `hooks validate` (parses a file), `serve`,
  `scheduler run`, `web`, `ssh-serve` (the faces a daemon host starts), `setup`, `upgrade`
  and `rollback` (this machine's binary), `sandbox-canary`, and `restore`, which runs
  wherever it is invoked, whatever `hosts.toml` names, because a lost daemon is the case it
  exists for and `packaging/recovery.sh` calls it with no flag an older binary would not know.
- **Local-only on purpose**: refused with a remote selected, and run on the daemon's host with
  `--local`. Each needs the host's keys, acts on what a restart of the host left, or would
  carry a credential over the API:
  - the host identity and keyring: `init`, `identity migrate`, `seal`, and the readers of
    this machine's keyring and store, `kit derive`, `kit verify` and `recovery-kit`. An
    offline `kit derive --identity --keyring` from a backup runs with `--local`.
  - the host's own jobs: `backup drill`, and `jobs redrive`, which re-drives what a restart of
    the host cut off.
  - credentials: `token` (the [threat model](../security/threat-model.md) keeps minting off
    `/api/v1`), `workspace token` and `workspace backend` (print a state-backend bearer),
    and `approval link` (a decision link is a single-use capability, which the [agent
    interface](../design/agent-interface.md) never hands to a machine face).
  - host-side machinery: `workspace adopt` (migrates a tofu dir's local state and serves the
    state backend itself), `litestream` (the sidecar beside the host's database), and `tui`
    (reads the data dir, and a client reaches a daemon's TUI over `ssh-serve`).
- **Pending**: it will act on the remote once its API route exists. Until then a selected
  remote refuses it with a message naming `--local`.

A refusal happens in the root hook before the command opens any data dir, and under
`--output json` it is a `wire` error envelope.

Rejected: forwarding the command line to the daemon and running the cobra tree there. Many
commands read stdin or local files, prompt, or exit the process with a job's code, and the
daemon would need a per-verb scope table anyway. Typed routes keep one service verb per
operation behind the faces that already exist (invariant 5, [Decision
0014](0014-agent-facing-surface.md)).

## Consequences

- A machine with a stored default now sends `playbook run`, `workspace plan`, `doctor` and
  every other remote-capable command to the daemon without a flag. The notice a bare run used
  to print ("running on this machine, not on the default remote") is gone with the case it
  described.
- On the daemon's own host, an operator whose `hosts.toml` names a default runs a local-only
  command with `--local`. A host with no `hosts.toml` behaves as before.
- A `hosts.toml` written before this change may already name a default, which the first
  `login` set when it only filled a valueless `--remote=`. From this change on that default
  applies to every command. A host that should stay local, such as a daemon host that logged
  in to itself for `--remote self drain`, removes the `default` line or logs in with
  `--no-default`.
- `upgrade` on a machine that only drives a remote daemon still writes this machine's data
  dir for its snapshot and step ledger.
- A pending command run against a configured remote fails until its route lands. `--local`
  still runs it against this machine's data dir, which is what it did before.
- Adding a command means classifying it, or the drift test fails.
- The pending set shrinks as routes land. When it is empty the guard still refuses the
  local-only set.
