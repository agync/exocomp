# workflows

Milestone workflows live here as `.js` Workflow scripts: they drive a
milestone from lane assignment through parallel implementation, adversarial
review, and integration back onto `main`. Named `mNN-build.js`, where `NN` is
the milestone number (`m0-build.js`, `m1-build.js`, ...), matching the
`mNN/<lane>` branch and `.worktrees/mNN-<lane>` naming used by the lanes
themselves.

Maintenance workflows live here too, for recurring upkeep that is not tied to
a milestone: dependency bumps, doc audits, hook or tooling refreshes. Named
`mNN-maintain.js`, where `NN` identifies the maintenance run so its output
stays traceable in `docs/log.md` the same way a milestone's is.
