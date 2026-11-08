#!/usr/bin/env bash
# Render the branded wiki figures under docs/design/assets/figures/.
#
# Two passes per figure: the page publishes its own height in <title>, then
# Chrome screenshots at exactly that height. A guessed height leaves a band or
# clips the card, so never set one by hand. The screenshot is converted to WebP
# at quality 80, matching the rest of the docs assets.
#
#   ./render.sh              # every figure
#   ./render.sh architecture-overview failover-sequence
#
# Requires google-chrome (or chromium) and ImageMagick. No network: the woff2
# faces the shell uses are vendored beside it.

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
out="$repo/docs/design/assets/figures"
mkdir -p "$out"

chrome=$(command -v google-chrome || command -v chromium || command -v chromium-browser || true)
if [[ -z $chrome ]]; then
  echo "render.sh: no chrome binary on PATH" >&2
  exit 1
fi

if command -v magick >/dev/null 2>&1; then
  imagemagick=(magick)
  identify_cmd=(magick identify)
elif command -v convert >/dev/null 2>&1; then
  imagemagick=(convert)
  identify_cmd=(identify)
else
  echo "render.sh: ImageMagick (magick or convert) is not on PATH" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if (($# > 0)); then
  sources=()
  for name in "$@"; do sources+=("$here/src/$name.html"); done
else
  shopt -s nullglob
  sources=("$here"/src/*.html)
fi

render_one() {
  local src=$1
  local name
  name=$(basename "$src" .html)
  local png="$tmp/$name.png"
  local webp_tmp="$tmp/$name.webp"

  local height
  if ! height=$("$chrome" --headless --disable-gpu --hide-scrollbars \
    --user-data-dir="$tmp/chrome-$name" \
    --disable-background-networking --disable-component-update --no-first-run \
    --virtual-time-budget=2500 --dump-dom "file://$src" 2>/dev/null |
    grep -oE '<title>H=[0-9]+' | grep -oE '[0-9]+' | head -1); then
    echo "render.sh: $name published no height" >&2
    return 1
  fi

  "$chrome" --headless --disable-gpu --hide-scrollbars \
    --user-data-dir="$tmp/chrome-$name" \
    --disable-background-networking --disable-component-update --no-first-run \
    --window-size=1600,"$height" --force-device-scale-factor=1 \
    --default-background-color=00000000 \
    --screenshot="$png" "file://$src" >/dev/null 2>&1

  "${imagemagick[@]}" "$png" -quality 80 "$webp_tmp"

  local dims w h bytes kb
  dims=$("${identify_cmd[@]}" -format '%w %h' "$webp_tmp")
  w=${dims% *}
  h=${dims#* }
  bytes=$(stat -c%s "$webp_tmp")
  kb=$(( bytes / 1024 ))
  printf '%-32s %sx%s  %s KB\n' "$(basename "$webp_tmp" .webp).webp" "$w" "$h" "$kb"

  if (( bytes > 1048576 )); then
    echo "render.sh: $(basename "$webp_tmp") exceeds the 1 MB pre-commit ceiling" >&2
    return 1
  fi
  if (( w * 10 < h * 12 || w * 10 > h * 28 )); then
    echo "render.sh: $(basename "$webp_tmp") has an awkward aspect ${w}x${h}" >&2
    return 1
  fi

  mv "$webp_tmp" "$out/$name.webp"
  rm -rf "$tmp/chrome-$name"
}

for src in "${sources[@]}"; do
  render_one "$src"
done
