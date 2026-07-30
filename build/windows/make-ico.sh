#!/bin/bash
# Rasterize the shared icon.svg into icon.ico, all the sizes Windows asks for.
#
# WHY THIS EXISTS AS A SCRIPT, and why the .ico is committed: exactly the
# reasons build/darwin/make-icns.sh gives. A normal build never runs this — it
# consumes the committed icon.ico — and getting a size wrong produces an icon
# that builds fine and looks generic in one specific place (the taskbar, or
# Alt-Tab, or the 16px Explorer list), which is a miserable thing to debug from
# a screenshot.
#
# The source is ../darwin/icon.svg, the same drawing macOS uses. One drawing,
# two containers: an icon that differed per platform would be a branding bug
# nobody notices until the two are seen side by side.
#
# The rasterizer is qlmanage — already on every Mac, no Homebrew dependency for
# something that runs once per icon change — and the .ico is assembled by the
# wails3 CLI, which is already required for GUI builds (AGENTS.md, Toolchain).
# Both of those make this script macOS-only, which is acceptable for a
# regeneration step and NOT acceptable for the build: the packaging Taskfile
# runs anywhere.
set -euo pipefail

cd "$(dirname "$0")"

SRC=../darwin/icon.svg
PNG=appicon.png

command -v qlmanage >/dev/null || { echo "qlmanage not found (macOS only)" >&2; exit 1; }
command -v wails3 >/dev/null || { echo "wails3 not found: go install github.com/wailsapp/wails/v3/cmd/wails3@latest" >&2; exit 1; }

# 1024 in, every smaller size derived by wails3. Rendering the largest once and
# downscaling beats rendering each size from the SVG: qlmanage's small renders
# lose the thin strokes, and a 16px icon is where that shows first.
qlmanage -t -s 1024 -o . "$SRC" >/dev/null 2>&1
mv "$(basename "$SRC").png" "$PNG"

# The default size set (256,128,64,48,32,16) is what Windows actually asks for:
# 256 for the Large Icons view, 48 for Medium, 32 for Alt-Tab and the title
# bar, 16 for Details view and the taskbar's small mode.
wails3 generate icons -input "$PNG" -windowsfilename icon.ico

rm -f "$PNG"

echo "wrote $(pwd)/icon.ico"
