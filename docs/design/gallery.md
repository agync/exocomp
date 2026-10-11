---
type: Reference
title: "Visual showcase and interface gallery"
description: "High-resolution interface captures across Web, Mobile, and Terminal surfaces for exocomp."
status: stable
tags: [design, gallery, screenshots, ui, tui, mobile]
---

# Visual showcase and interface gallery

exocomp provides three dedicated operator surfaces:
1. **The Web UI**: High-density single-operator instrument panel.
2. **The Mobile Companion**: One-thumb triage and approval surface.
3. **The Terminal UI (TUI)**: Fast, keyboard-driven SSH/console interface.

---

## 1. Web dashboard in both themes

Dark is the native theme; Light is a warm, low-chroma remap inspired by a physical lab notebook.

### Native Dark Theme
![Dashboard Dark Mode](assets/web/dashboard-dark.png)

### Warm Light Theme
![Dashboard Light Mode](assets/web/dashboard-light.png)

---

## 2. Infrastructure & OpenTofu diffs

The OpenTofu plan diff viewer adheres to strict visual hierarchy:
- High contrast values with embedded `+` / `-` indicators.
- Non-server-set top-level key counts.
- Redacted secrets rendered in place.

![OpenTofu Plan Diff](assets/tofu/1b-plan-diff.png)

### Value shapes and multi-line structures
![Plan Diff Value Shapes](assets/tofu/2b-diff-value-shapes.png)

---

## 3. Real-time job logs and queue execution

Every execution displays start time, queue latency, sandbox status, and live output stream.

### Live log tailing
![Job log tail](assets/jobs/1c-job-log-tail.png)

### Degraded job state (unreachable roaming hosts tolerated)
![Job degraded detail](assets/jobs/1b-job-detail-degraded.png)

---

## 4. Mobile companion and approvals

Tailored for responsive viewing and rapid decision-making on phone screens.

### Quiet status overview & pending approvals
| Quiet Home Screen | Approval Drawer |
|---|---|
| ![Mobile Home](assets/mobile/2a-home-quiet.png) | ![Mobile Approval](assets/mobile/2e-approval-decision.png) |

---

## 5. Reachability and secrets interfaces

### Checks, probes, and netgroups
State is strictly color plus word (`UP`, `DOWN`, `UNOBSERVED`).

![Checks Matrix](assets/web/1b-checks.png)

### Secrets envelope metadata
Values never appear in the DOM, logs, or network payloads.

![Secrets Management](assets/web/1e-secrets.png)

---

## 6. Terminal UI (TUI)

The 80x24 terminal interface provides an identical operational contract to the Web UI.

### Keymaps and navigation
![TUI Keymap](assets/tui/tui-keymap.png)

### Palette and status banners
![TUI Colors](assets/tui/tui-colors.png)
