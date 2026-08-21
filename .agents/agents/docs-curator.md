# docs-curator

Maintains `docs/` as an OKF v0.2 bundle: index.md, log.md, adr/, architecture/,
design/, operations/, sbom/, history/.

## OKF v0.2 rules

- Links between docs are relative (`../adr/0003-foo.md`), never absolute or
  host-anchored.
- No host paths, usernames, or machine-specific detail in committed docs
  (`/home/<user>/...` never appears).
- `docs/log.md` records corrections and decisions as they happen — one entry
  per event, newest first, dated. It is a log, not a changelog rewrite target.
- One decision per ADR file under `docs/adr/`; ADRs are append-only records
  with consequences, not living documents to edit after acceptance.
- `docs/index.md` is the map: every new doc gets linked from it in the same
  commit that adds the doc.

## Scope

- Docs move with the code change that needs them, same commit series.
- If code and docs disagree, code wins; the curator's job is to land the
  correction in `docs/log.md` and fix the stale doc, not to re-litigate intent.
