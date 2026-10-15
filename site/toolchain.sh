#!/usr/bin/env bash
# Lays down the render toolchain in site/.toolchain for a host with no container
# runtime, which is what a CI build image is: the okf-wiki binary built from
# source, plus the pinned mermaid runtime the renderer needs because the docs
# bundle has diagrams. build.sh picks the directory up on its own.
#
# OKF_WIKI_REF pins the source (a tag or commit); the default tracks the
# module's default branch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="github.com/abn/okf-wiki"
REF="${OKF_WIKI_REF:-latest}"
OUT="$SCRIPT_DIR/.toolchain"

echo "==> Building the render toolchain in $OUT..."

echo "==> 1/2 Installing okf-wiki ($MODULE@$REF)..."
mkdir -p "$OUT/bin"
GOBIN="$OUT/bin" go install "$MODULE/cmd/okf-wiki@$REF"

echo "==> 2/2 Building the mermaid runtime..."
# The module cache is read-only and npm ci writes, so the files that describe
# the bundle are copied out; nothing else of the module is kept.
src="$(go list -m -f '{{.Dir}}' "$MODULE@$REF")"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp "$src/mermaid/package.json" "$src/mermaid/package-lock.json" \
  "$src/mermaid/entry.js" "$work/"
mkdir -p "$OUT/mermaid"
cd "$work"
npm ci --no-audit --no-fund --legacy-peer-deps
./node_modules/.bin/esbuild entry.js --bundle --format=esm --minify \
  --outfile="$OUT/mermaid/mermaid-bundle.min.mjs"

echo "==> Toolchain ready: $OUT"
