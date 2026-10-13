#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "==> Building exocomp site..."

mkdir -p "$SCRIPT_DIR/dist"

echo "==> 1. Rendering docs with okf-wiki..."
podman run --rm --user 0:0 \
  -v "$REPO_ROOT/docs:/content:ro,Z" \
  -v "$SCRIPT_DIR/theme:/theme:ro,Z" \
  -v "$SCRIPT_DIR/dist:/out:Z" \
  -e OKF_WIKI_THEME=/theme \
  -e OKF_WIKI_BRAND="exocomp" \
  -e OKF_WIKI_BRAND_SUB="docs" \
  -e OKF_WIKI_TITLE="exocomp knowledge base" \
  ghcr.io/abn/okf-wiki:latest render --content /content --out /out --base /docs/ --nav-links "Website=https://exocomp.dev/,GitHub=https://github.com/abn/exocomp"

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
