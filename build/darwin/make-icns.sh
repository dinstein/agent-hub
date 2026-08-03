#!/bin/bash
# Rasterize icon.svg into icons.icns, all the sizes macOS asks for.
#
# WHY THIS EXISTS AS A SCRIPT. iconutil needs a .iconset directory with exactly
# these ten names; getting one wrong produces an .icns that builds fine and
# then shows a generic document icon at one specific size, which is a miserable
# thing to debug from a screenshot.
#
# The rasterizer is qlmanage — already on every Mac, no Homebrew dependency for
# something that runs once per icon change. It writes <name>.png next to its
# input and ignores -o for SVGs, hence the moves below.
#
# The .icns is committed, so a normal build never runs this. Re-run it only
# after editing icon.svg.
set -euo pipefail

cd "$(dirname "$0")"

SRC=icon.svg
ISET=AgentHub.iconset

command -v iconutil >/dev/null || { echo "iconutil not found (macOS only)" >&2; exit 1; }

# Parse the source before rasterizing it. qlmanage answers malformed XML with a
# rendered error page and exit status 0, so without this check a stray double
# hyphen inside a comment ships as the application icon and the first sign of
# it is a pink box in the Dock.
xmllint --noout "$SRC" || { echo "$SRC is not well-formed XML; not rasterizing it" >&2; exit 1; }

rm -rf "$ISET" && mkdir -p "$ISET"

# name:pixels — the @2x entries are the same pixel count as the next size up,
# which is why 32 and 64 and 256 and 512 each appear twice.
render() {
  local px=$1 out=$2
  qlmanage -t -s "$px" -o . "$SRC" >/dev/null 2>&1
  # qlmanage pads to a square of the requested size; sips guarantees the exact
  # dimensions iconutil demands rather than trusting that padding.
  sips -z "$px" "$px" "$SRC.png" --out "$ISET/$out" >/dev/null
  rm -f "$SRC.png"
}

render 16    icon_16x16.png
render 32    icon_16x16@2x.png
render 32    icon_32x32.png
render 64    icon_32x32@2x.png
render 128   icon_128x128.png
render 256   icon_128x128@2x.png
render 256   icon_256x256.png
render 512   icon_256x256@2x.png
render 512   icon_512x512.png
render 1024  icon_512x512@2x.png

iconutil -c icns "$ISET" -o icons.icns
rm -rf "$ISET"

echo "wrote $(pwd)/icons.icns"
