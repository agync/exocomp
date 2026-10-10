# Design

What the project is for, the users it targets, and the order in which it gets built.

| Page | Covers |
|---|---|
| [Requirements](requirements.md) | The two hard constraints, the six goals, the anti-scope contract, and what is deferred with a named seam |
| [Personas](personas.md) | The four target users: estate, goals, and the constraint that decides adoption |
| [Amendments](amendments.md) | The twelve normative changes that override the candidate design wherever they differ |
| [Delivery](delivery.md) | Milestones v0.1 / v0.5 / v1.0 with effort, and the risks carried into each |
| [v1 acceptance bar](v1-acceptance.md) | The five end-to-end stories that gate v1.0 |
| [v1.1+ roadmap](v1.1-roadmap.md) | The ranked backlog after v1.0 (generic CLI, drift/deadline signals, rotation, rolling strategy, self-DR) |
| [Agent interface](agent-interface.md) | The three programmatic faces (remote CLI, HTTP `/api/v1`, MCP), the wire envelope, and the error/exit-code taxonomy |
| [Bootstrap-first chain](bootstrap-first.md) | The ansible-first-then-tofu handoff for the Kubernetes case, and its credential capture |
| [BYO runner](byo-runner.md) | Why the container/bring-your-own-runner tier stays deferred, its shape if built, and the per-job resource caps that are built |
| [Check-triggered runs](check-triggered-runs.md) | Checks (named probe-sets), network groups (named CIDR sets), and the governed `playbook` rule action that let a run trigger on a reachability or health transition |
| [UI brief](ui-brief.md) | Design brief for the web UI and phone approval surface |
| [TUI brief](tui-brief.md) | Design brief for the terminal surface: constraints, screens, deliverables |
| [Design invariants](INVARIANTS.md) | The design philosophy, hard rules, and component vocabulary that survive any redesign |
| [Web design tokens](../internal/web/tokens.css) | The tokens every web surface renders from |
| [TUI design notes](reference/tui/) | Keymap, colors, empty-state copy, and states; the golden frames live at `internal/tui/testdata/frames/` |
| [First-run walkthrough](first-run-walkthrough.html) | Static HTML mockup of the init → onboard → first run flow on the design tokens |

The accepted design is thin-core ([ADR 0001](../adr/0001-thin-core-architecture.md)). The
three candidates it was chosen from, the critique that reshaped them, and the research notes
behind them are archived in the exocomp design phase of the lab wiki.
