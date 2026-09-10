---
name: web-demo-recording
description: Record a reusable, high-quality browser demo of any exocomp web surface — terraform/tofu runs, ansible playbooks, forms, approvals, or a whole feature — as a native-1080p WebM with screenshots, then produce a tight action-only cut. Covers the TLS daemon + tailscale HTTPS chain (no insecure warning), the dark-theme switch, the video-start --size pixelation fix, screenshot capture, the step-cut trim, and frame-level verification. Use whenever a demo recording or a set of web screenshots is needed, regardless of the feature being shown.
---

# Web demo recording

Record a shareable exocomp browser demo: native 1080p WebM + screenshots, then a
tight step cut. Proven on the k3s tofu-surfaces demo (v1→v7). The recipe is
feature-agnostic — swap in whatever surface you're showing.

## Fast path (the whole loop)

```sh
# 1. Env: TLS daemon + tailscale HTTPS (no insecure warning)
make build
EXOCOMP_E2E_TLS=1 EXOCOMP_BIN=bin/exocomp .contrib/tofu-e2e/lib/daemon.sh start
timeout 30 tailscale serve --https 8643 --bg --yes https+insecure://127.0.0.1:8642 </dev/null
# (register your workspace/seed your data, then:)
#   read the operator token from .contrib/tofu-e2e/lib/daemon.sh's tokens.env
#   (secret = exo_<id>_<secret>, NOT the token NAME)

# 2. Record (native 1080p — the --size is the pixelation fix)
playwright-cli open --browser=chrome
playwright-cli resize 1920 1080
playwright-cli video-start <name> --size 1920x1080
playwright-cli video-chapter "N. Title" --description="..."   # mark each beat

# 3. Dark theme as a real on-camera switch
playwright-cli eval "() => localStorage.setItem('exocomp-theme','light')"
playwright-cli reload          # button now reads "Switch to dark"
# ...ONE click on the toggle mid-demo...

# 4. Drive the demo; screenshot each step
playwright-cli screenshot --filename=<dir>/<step>.png

# 5. The video lands at CWD, NOT .playwright-cli/ — move it
mv <name> .agents/brain/screenshots/<proj>/<name>.webm

# 6. Tight step cut (see references/trimming.md)
# 7. Verify (see references/verification.md) — I cannot view video, so check
#    the theme arc + frame content by numbers, then eyeball before sharing.
```

## Hard rules

- **`video-start --size 1920x1080`** — the default recorder fits within 800x800
  → pixelated. `--size` records natively. Never upscale a soft source; the
  sharp path is native capture + ffmpeg speed-up (no scale filter).
- **TLS, not cleartext.** `EXOCOMP_E2E_TLS=1` serves real TLS (self-signed) so
  the login page shows the cert fingerprint, NOT the "not secure" alert; the
  tailscale `https+insecure://` proxy then gives the browser a valid tailnet
  cert end-to-end. `--bg` is required (foreground dies with the command) and
  stdin from `/dev/null` avoids the hang.
- **Clean the k8s cluster first.** The kubernetes provider refuses to adopt a
  pre-existing namespace: `kubectl delete namespace <ns> --ignore-not-found`.
- **The token is the secret**, not the name. `tokens.env` exports
  `EXOCOMP_OPERATOR_TOKEN=exo_<id>_<secret>` — paste the whole `exo_...` value.
- **Confirm dialogs are native `<dialog>`s** — click the visible
  "Queue apply — …" button, not `dialog-accept`.
- **Screenshots are the ground truth** for theme/steps; the video can drift
  (a mid-recording light excursion, a reload flash). If they disagree, trust the
  screenshots and re-cut the affected step window.

## The step cut (trim between actions)

Frame-diff can't separate "action" from "log streaming" (both are near-static).
The reliable anchors are the **screenshots** you took per step. Build keep-windows
around each step's video time and select-concat them (see
`references/trimming.md` for the script and the re-anchoring gotcha). The result
drops the waits between actions — a 18-min raw becomes ~2 min.

## Verification (I can't see video — check by numbers)

`references/verification.md` has the exact ffprobe/identify checks: duration,
frame count, and a brightness arc that must read login-light (mean ~239) then
dark (~16-19) throughout. A mean ~255 mid-video means a light excursion or
reload flash — re-cut that window. Then eyeball it before sharing.

## Cleanup

`playwright-cli close` · kill the daemon · `tailscale serve --https=8643 off`
(leaves existing 443/8443 endpoints untouched) · leave the cluster restored.

## References

- `references/environment.md` — TLS daemon + tailscale chain, token, seeding.
- `references/trimming.md` — the step-cut script, windows, re-anchoring.
- `references/screenshots.md` — capture workflow + naming.
- `references/verification.md` — the ffprobe/identify checks.
