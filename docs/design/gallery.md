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
<p><img src="assets/web/dashboard-dark.png" alt="Dashboard Dark Mode"></p>

### Warm Light Theme
<p><img src="assets/web/dashboard-light.png" alt="Dashboard Light Mode"></p>

---

## 2. Infrastructure & OpenTofu diffs

The OpenTofu plan diff viewer adheres to strict visual hierarchy:
- High contrast values with embedded `+` / `-` indicators.
- Non-server-set top-level key counts.
- Redacted secrets rendered in place.

<p><img src="assets/tofu/1b-plan-diff.png" alt="OpenTofu Plan Diff"></p>

### Value shapes and multi-line structures
<p><img src="assets/tofu/2b-diff-value-shapes.png" alt="Plan Diff Value Shapes"></p>

---

## 3. Real-time job logs and queue execution

Every execution displays start time, queue latency, sandbox status, and live output stream.

### Live log tailing
<p><img src="assets/jobs/1c-job-log-tail.png" alt="Job log tail"></p>

### Degraded job state (unreachable roaming hosts tolerated)
<p><img src="assets/jobs/1b-job-detail-degraded.png" alt="Job degraded detail"></p>

---

## 4. Mobile companion and approvals

Tailored for responsive viewing and rapid decision-making on phone screens.

### Quiet status overview & pending approvals
| Quiet Home Screen | Approval Drawer |
|---|---|
| <img src="assets/mobile/2a-home-quiet.png" alt="Mobile Home"> | <img src="assets/mobile/2e-approval-decision.png" alt="Mobile Approval"> |

---

## 5. Reachability and secrets interfaces

### Checks, probes, and netgroups
State is strictly color plus word (`UP`, `DOWN`, `UNOBSERVED`).

<p><img src="assets/web/1b-checks.png" alt="Checks Matrix"></p>

### Secrets envelope metadata
Values never appear in the DOM, logs, or network payloads.

<p><img src="assets/web/1e-secrets.png" alt="Secrets Management"></p>

---

## 6. Terminal UI (TUI)

The 80x24 terminal interface provides an identical operational contract to the Web UI.

### Keymaps and navigation
<p><img src="assets/tui/tui-keymap.png" alt="TUI Keymap"></p>

### Palette and status banners
<p><img src="assets/tui/tui-colors.png" alt="TUI Colors"></p>
