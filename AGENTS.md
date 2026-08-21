# exocomp

Homelab control plane: one rootless container holding one Go binary (`exocompd`) that
owns state, sequencing, keys, and recovery, shelling out to pinned tools
(ansible-core, opentofu, restic, sops, age, git) for everything else.

This file is the entrypoint and contract for every agent and human working here.
Start with `docs/index.md` for architecture, ADRs, and operations.

## Invariants

Breaking any of these fails review, regardless of how good the change is otherwise.

1. One operator, one container, no external database — ever.
2. Nothing is stored in a bespoke format: plain SQLite, plain restic repos, plain git
   bundles, plain age files, standard tofu state.
3. Subprocess correctness is decided by exit codes only; parsed output enriches the UI.
4. Secrets never appear in logs, job events, docs, commits, or test fixtures.
5. Every execution path goes through the one job queue; guardrails live in the queue,
   not in configuration.
6. Nothing here runs sudo. Work needing elevation becomes an explicit request to the
   operator.
7. Disaster recovery never depends on a registry, a vendor, or this project's own code.
8. Docs move with the change: `docs/` is an OKF v0.2 bundle; update it in the same
   commit series as the code it describes.
9. Commits are logical units: conventional style, title <= 52 chars, terse body saying
   what the change does. No trailers. No `git add -A`. No meta-messages.
10. No AI slop: comments only for constraints the code cannot show; no marketing
    language anywhere; no filler prose in docs.
11. Parallel work happens in worktrees under `.worktrees/`; `main` stays linear
    (rebase then fast-forward; merge commits only for milestone integrations).
12. Automation over manual conformance: `make check` gates every commit via the hooks
    in `.agents/hooks/` (`git config core.hooksPath .agents/hooks` after clone).

## Layout

```
AGENTS.md        this contract
.agents/         agents/ skills/ workflows/ hooks/ — automation lives here
docs/            OKF v0.2 bundle: index.md, log.md, adr/, architecture/,
                 design/, operations/, sbom/, history/
cmd/exocomp/     the single cobra binary (`exocompd` = alias for `exocomp serve`)
internal/        service, api, queue, secrets, executor, tui, web
api/             OpenAPI 3.1 spec + typed client package (the monorepo seam)
packaging/       Containerfile, quadlet, compose, install.sh (separately versioned)
test/lima/       Fedora lima templates for dummy managed nodes + smoke tests
```

## Working here

- Read `docs/index.md` before changing architecture-relevant code; if docs and code
  disagree, code wins and the correction lands in `docs/log.md`.
- Verification before completion: run the check, paste the output, then claim done.
- ADRs record decisions with consequences; one decision per file under `docs/adr/`.
