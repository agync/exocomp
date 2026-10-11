---
type: Reference
title: "Web design system"
description: "Color tokens, typography, dark/light theme remapping, and contrast invariants for the exocomp web surface."
status: stable
tags: [design, web, tokens, css]
---

# Web design system

The exocomp web surface renders from the design tokens defined in `internal/web/tokens.css` and the design authority `DESIGN.md`.

It represents a single-operator instrument panel engineered for high information density, strict contrast ratios, and low visual fatigue.

## Themes

The interface supports two complete color themes:
- **Dark (native)**: `#0A0C0D` void backplate. Optimized for OLED monitors and terminal parity.
- **Light (warm remap)**: `#F5F2EC` void backplate. A warm, low-chroma palette styled after physical laboratory notebooks.

The theme toggle cycles through `System → Light → Dark`. Contrast ratios are measured strictly against the tray surface where tokens sit (`#171C1E` in dark, `#FFFDF9` in light).

## Core invariants

1. **`ink-600` is never applied to text**:
   `ink-600` (`#6B7477` in dark, `rgba(60,50,38,.28)` in light) has a contrast ratio of 3.6:1. It is strictly constrained to decorative structural elements (dots, rules, dividers). Applying `ink-600` to readable text fails WCAG AA standards and is a review violation.
2. **`font-variant-numeric: tabular-nums` is mandatory for numbers**:
   All digits (timestamps, exit codes, resource counts, byte sizes) must align vertically. CSS `font` shorthand declarations must never be placed after `tabular-nums` on matching selectors, as the `font` shorthand resets all variant properties.
3. **Status before detail**:
   The primary dashboard screen must resolve whether the estate is healthy in under ten seconds without scrolling.
4. **State is colour plus word**:
   No UI indicator relies solely on color. A red chip must include the textual state `FAIL` or `DOWN`.
5. **Truth over reassurance**:
   No animated loading spinners, fake progress bars, or vanity metrics. Timestamps, exit codes, and byte counts are shown plainly.

## Palette tokens summary

| Token | Dark role | Light role | Contrast (Dark) |
|---|---|---|---|
| `--surface-void` | `#0A0C0D` | `#F5F2EC` | Backplate |
| `--surface-rail` | `#0E1112` | `#EFEBE3` | Left navigation |
| `--surface-card` | `#121618` | `#FFFDF9` | Card and tables |
| `--surface-tray` | `#171C1E` | `#FFFDF9` | Raised group tray |
| `--ink-100` | `#ECEFF0` | `#1D1A16` | 14.6:1 (Headings) |
| `--ink-200` | `#D6DBDD` | `#3A352E` | 11.7:1 (Body text) |
| `--ink-400` | `#A6AFB2` | `#6B655B` | 7.5:1 (Secondary) |
| `--ink-500` | `#8A9296` | `#6B655B` | 5.4:1 (Timestamps) |
| `--ink-600` | `#6B7477` | `rgba(60,50,38,.28)` | 3.6:1 (Rules/dots only) |
| `--signal-ok` | `#4FB86B` | `#2D8A46` | 7.9:1 (Healthy) |
| `--signal-wait` | `#E3A008` | `#A36B00` | 7.6:1 (Approval/drift) |
| `--signal-fail` | `#F0645A` | `#C8372D` | 5.4:1 (Failure/error) |
