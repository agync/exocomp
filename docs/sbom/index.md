# Software bill of materials

| Page | Covers |
|---|---|
| [Bill of materials](bill-of-materials.md) | The five layers — linked-in Go libraries, pinned subprocess tools, deployment substrate, operator-supplied services, and the budget rollup — plus the GPL subprocess-boundary rule |

Two rules govern everything listed there:

- **GPL crosses a process boundary or does not ship.** `ansible-core` and `git` are
  subprocesses, never linked, so `exocompd`'s own license stays unencumbered.
- **Pins are recorded per build in `MANIFEST.json`**, not in prose. `exocomp doctor`
  enforces them and refuses to run a job against an untested tool version.
