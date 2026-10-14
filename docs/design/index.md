# Design system and surfaces

exocomp treats user interface design as a functional contract: status before detail, failure louder than success, and zero decoration that does not inform the operator.

Dark is the native theme; Light is a warm, low-chroma remap inspired by physical laboratory notebooks.

## Visual showcase and guides

| Page | Covers |
|---|---|
| [Visual showcase & gallery](gallery.md) | **[FRONT & CENTER]** High-resolution screenshots of the Web Dashboard (dark & light), OpenTofu plan diffs, live log tails, checks matrix, and TUI |
| [Design invariants](INVARIANTS.md) | The five immutable design principles, color-plus-word rules, and UI vocabulary |
| [Web design system](web-system.md) | CSS design tokens, typography scale, tabular number invariants, and WCAG AA contrast rules |
| [Terminal UI (TUI)](tui-system.md) | Bubble Tea/Lip Gloss terminal surface, 80x24 layout, keymaps, and Row 24 powerline strip |
| [Machine token specification](DESIGN.md) | Full token catalog transcribed from `internal/web/tokens.css` for automated agents |

## Foundational design decisions

The accepted design is thin-core ([ADR 0001](../adr/0001-thin-core-architecture.md)). Historical design phase research notes and candidate critiques are archived in the exocomp section of the lab wiki.
