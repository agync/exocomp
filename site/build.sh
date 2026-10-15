#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Two renderers, same output: the pinned image under podman (the reference
# render, used wherever podman is installed) or a standalone okf-wiki binary,
# which is what a CI build image with no container runtime can run. The binary
# comes from OKF_WIKI_BIN, from site/.toolchain (toolchain.sh), or from PATH.
IMAGE="${OKF_WIKI_IMAGE:-ghcr.io/abn/okf-wiki:latest}"
TOOLCHAIN="$SCRIPT_DIR/.toolchain"
BRAND="exocomp"
BRAND_SUB="docs"
TITLE="exocomp knowledge base"
BASE="/docs/"
NAV_LINKS="Website=https://exocomp.dev/,GitHub=https://github.com/abn/exocomp"

BIN="${OKF_WIKI_BIN:-}"
if [ -z "$BIN" ] && ! command -v podman >/dev/null 2>&1; then
  if [ -x "$TOOLCHAIN/bin/okf-wiki" ]; then
    BIN="$TOOLCHAIN/bin/okf-wiki"
  elif command -v okf-wiki >/dev/null 2>&1; then
    BIN="$(command -v okf-wiki)"
  fi
fi

echo "==> Building exocomp site..."

mkdir -p "$SCRIPT_DIR/dist"

echo "==> 1. Rendering docs with okf-wiki..."
if [ -n "$BIN" ]; then
  [ -x "$BIN" ] || { echo "error: OKF_WIKI_BIN=$BIN is not executable" >&2; exit 1; }
  # A bundle with a diagram and no mermaid runtime is an error rather than a
  # quiet CDN fallback, so a binary render passes --vendor when the toolchain
  # has laid one down.
  VENDOR="${OKF_WIKI_VENDOR:-$TOOLCHAIN/mermaid}"
  render_args=(--content "$REPO_ROOT/docs" --out "$SCRIPT_DIR/dist" --base "$BASE"
    --theme "$SCRIPT_DIR/theme" --brand "$BRAND" --brand-sub "$BRAND_SUB"
    --title "$TITLE")
  if [ -f "$VENDOR/mermaid-bundle.min.mjs" ]; then
    render_args+=(--vendor "$VENDOR")
  fi
  render_args+=(--nav-links "$NAV_LINKS")
  "$BIN" render "${render_args[@]}"
else
  command -v podman >/dev/null 2>&1 || {
    echo "error: no renderer: install podman, run site/toolchain.sh, or set OKF_WIKI_BIN" >&2
    exit 1
  }
  podman run --rm --user 0:0 \
    -v "$REPO_ROOT/docs:/content:ro,Z" \
    -v "$SCRIPT_DIR/theme:/theme:ro,Z" \
    -v "$SCRIPT_DIR/dist:/out:Z" \
    -e OKF_WIKI_THEME=/theme \
    -e OKF_WIKI_BRAND="$BRAND" \
    -e OKF_WIKI_BRAND_SUB="$BRAND_SUB" \
    -e OKF_WIKI_TITLE="$TITLE" \
    "$IMAGE" render --content /content --out /out --base "$BASE" \
    --nav-links "$NAV_LINKS"
fi

echo "==> 2. Copying landing page and static assets..."
cp "$SCRIPT_DIR/index.html" "$SCRIPT_DIR/dist/index.html"
cp -r "$SCRIPT_DIR/styles" "$SCRIPT_DIR/dist/"
cp -r "$SCRIPT_DIR/js" "$SCRIPT_DIR/dist/"
cp -r "$SCRIPT_DIR/assets" "$SCRIPT_DIR/dist/"
cp "$SCRIPT_DIR/_headers" "$SCRIPT_DIR/dist/"
cp "$SCRIPT_DIR/_redirects" "$SCRIPT_DIR/dist/"
cp "$SCRIPT_DIR/robots.txt" "$SCRIPT_DIR/dist/"
cp "$SCRIPT_DIR/sitemap.xml" "$SCRIPT_DIR/dist/"

echo "==> Build complete! Output located at site/dist"
echo "    - Landing page: site/dist/index.html"
echo "    - Docs root:    site/dist/docs/index.html"
