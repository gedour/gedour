#!/usr/bin/env bash
# Renders .github/banner-src.svg into .github/banner.svg with every text outlined, so GitHub shows
# Bricolage Grotesque without loading a font. Needs inkscape, python3 with fontTools (brotli for woff2)
# and web/node_modules (pnpm -C web install).
# Usage: scripts/render-banner.sh [repo root]
set -euo pipefail

readonly ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
readonly SRC="$ROOT/.github/banner-src.svg"
readonly OUT="$ROOT/.github/banner.svg"
readonly FONT="$ROOT/web/node_modules/@fontsource-variable/bricolage-grotesque/files/bricolage-grotesque-latin-wght-normal.woff2"

for cmd in inkscape python3 fc-list; do
  if ! command -v "$cmd" >/dev/null; then
    echo "render-banner: $cmd not found" >&2
    exit 1
  fi
done
for f in "$SRC" "$FONT"; do
  if [[ ! -f "$f" ]]; then
    echo "render-banner: missing $f" >&2
    exit 1
  fi
done

# Inkscape outlines an unknown family with a fallback font and says nothing, so check the source first.
# grep -v exits 1 when every family is known, which is the success case.
unknown="$(grep -oE 'font-family(="[^"]*"|:[^;"]*)' "$SRC" | sed -E 's/^font-family[=:]"?//; s/"$//' |
  sort -u | grep -vxE 'Brico(600|800)' || true)"
if [[ -n "$unknown" ]]; then
  echo "render-banner: $SRC uses families this script does not build: $unknown" >&2
  exit 1
fi

tmpdir="$(mktemp -d -t gedour-banner.XXXXXX)"
trap 'rm -rf "$tmpdir"' EXIT

# The source names families Brico600/Brico800: fontconfig lists the variable font under one odd family
# name, so each weight the banner uses is pinned into a static font with its own family.
python3 - "$FONT" "$tmpdir" <<'EOF'
import sys
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

font, out = sys.argv[1], sys.argv[2]
for weight in (600, 800):
    t = instantiateVariableFont(TTFont(font), {"wght": weight})
    family = f"Brico{weight}"
    names = t["name"]
    for nid in (16, 17):
        names.removeNames(nameID=nid)
    names.setName(family, 1, 3, 1, 0x409)
    names.setName("Regular", 2, 3, 1, 0x409)
    names.setName(f"{family} Regular", 4, 3, 1, 0x409)
    names.setName(f"{family}-Regular", 6, 3, 1, 0x409)
    t.flavor = None
    t.save(f"{out}/{family}.ttf")
EOF

cat >"$tmpdir/fonts.conf" <<EOF
<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "fonts.dtd">
<fontconfig><dir>$tmpdir</dir><cachedir>$tmpdir/cache</cachedir></fontconfig>
EOF

families="$(FONTCONFIG_FILE="$tmpdir/fonts.conf" fc-list : family)"
for family in Brico600 Brico800; do
  if ! grep -qx "$family" <<<"$families"; then
    echo "render-banner: fontconfig does not see $family" >&2
    exit 1
  fi
done

FONTCONFIG_FILE="$tmpdir/fonts.conf" inkscape "$SRC" --export-text-to-path --export-plain-svg \
  --export-filename="$OUT"

echo "render-banner: wrote $OUT"
