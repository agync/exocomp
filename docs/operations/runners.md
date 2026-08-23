# Runner definitions (registry only — the build tier is deferred)

`exocomp runner` registers execution-environment **recipes** in the database. Registering
one stores a definition; it does not build an image, call podman, or hand anything to a
job. Every runner verb repeats this plainly: **"registered for the deferred container tier;
not yet executed."** The shape follows
[design/byo-runner.md](../design/byo-runner.md) §2, which also argues the deferral itself
is correct, not just unfinished.

## Why register something that does nothing yet

The registry is the two-tier model dependencies already use: an operator can name a runner
once (`runner add`), a source can reference it, and `runner import` loads GitOps-style
`runner.yaml` specs from a directory into the same database — all before there is a build
tier to consume any of it. A definition is inert data: no secret material, no execution
side effect, so there is nothing here for the sandbox, the queue, or the approval gate to
guard yet.

## Registering a runner

```
exocomp runner add fedora-ee --base registry.fedoraproject.org/fedora-minimal:44 \
  --packages sshpass,rsync
```

Two mutually exclusive build models feed the **same** content hash, so switching between
them never changes a runner's identity:

- **Model A (`--mode cached`, default)** — `--base` plus `--bootstrap-file` (a script run as
  one layer) and/or `--packages` (sorted, deduplicated).
- **Model B (`--mode onthefly`)** — `--base` plus `--containerfile <path>`. Naming a
  Containerfile selects `onthefly` by default unless `--mode` is passed explicitly.

Other flags: `--ansible-core <version>` (a per-project ansible-core pin), `--memory-max` /
`--cpu-quota` (the resource ceilings a run *would* ask for, per design §6 — nothing enforces
them today).

`runner list` / `runner show <name>` / `runner rm <name>` round out the registry; `show`
prints the bootstrap script body on the text path (`--output json` never carries it — the
JSON projection stays body-free, since an arbitrarily long script would blow the envelope's
low-token contract for every listing; the response carries only its SHA-256 and line count).

## Importing specs from a directory

```
exocomp runner import ./runners
```

Reads every `*.yaml`/`*.yml` in the directory as a `runner.yaml` spec (with or without the
`runner:` wrapper design §2 shows), creating or updating the runner of that name. A
malformed or invalid spec fails the import at that file, before any later file in the
directory is applied. A `build.no_cache` field in an imported spec is accepted but has no
effect yet, and the import says so.

## Naming

A runner name is 1–64 characters, must start with a lowercase letter or digit, and holds
only `[a-z0-9._-]` — the shape an eventual image tag would need, enforced at registration
rather than left to fail later at a build that does not exist yet.

## Where a definition lives, and why

Definitions live in `internal/store` — the same database as sources, inventories,
schedules, and plan-policies — so a runner is backed up and audited with the data volume,
not held in a config tree the volume does not carry. `runner import` reads from a directory
outside the data dir but writes into it; nothing under `/etc` or `~/.config` holds runner
state (`~/.config/exocomp` is reserved for the remote CLI's own client-side host pins, the
opposite direction: a workstation reaching a daemon, not the daemon's own registry).

Related: [design/byo-runner.md](../design/byo-runner.md) for the full build-mode design,
the tooling decision (shell podman, never embed the Go bindings), and the phased plan for
if and when the build tier is actually built.
