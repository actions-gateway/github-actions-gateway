#!/usr/bin/env bash
#
# Render the animated wormhole logomark.
#
# Produces two artefacts from generate-wormhole-animation.py's frame SVGs:
#
#   1. wormhole-animation.webp  — a light (~600 KB), TRANSPARENT, looping WebP
#      that IS committed and used in the README footer and the docs 404 page. The
#      cyan plume + blue ring read on both a light and a dark page, so one alpha
#      asset works in either theme (no baked-in backdrop needed).
#
#   2. <OUTDIR>/wormhole-animation.mp4 — a full-fidelity, tighter-cropped video
#      for social upload (e.g. Bluesky/X, which animate where an uploaded
#      GIF/WebP would be static). Opaque (no alpha in H.264). NOT committed.
#
# Deps: python3, resvg, ImageMagick 7 (`magick`), img2webp (libwebp), ffmpeg.
#       brew install resvg imagemagick webp ffmpeg
#
# Usage: ./render-wormhole-animation.sh [OUTDIR]   # MP4 OUTDIR defaults to <repo>/tmp/wormhole/ (gitignored)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly GEN="${SCRIPT_DIR}/generate-wormhole-animation.py"
readonly WEBP="${SCRIPT_DIR}/wormhole-animation.webp"   # committed, light, transparent
# The MP4 isn't committed, so it defaults into the repo's gitignored tmp/; pass
# an explicit OUTDIR to override. Falls back to $PWD outside a git checkout.
repo_root="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel 2>/dev/null || echo "${PWD}")"
readonly OUTDIR="${1:-${repo_root}/tmp/wormhole}"
readonly ASPECT="190.5"        # frame width:height is ASPECT:100 (1.91:1)

work=""                        # scratch dir; cleaned up on exit
cleanup() {
  if [[ -n "${work}" ]]; then
    rm -rf "${work}"
  fi
}
trap cleanup EXIT

require() {
  local cmd
  for cmd in "$@"; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      echo "error: required command '${cmd}' not found on PATH" >&2
      exit 1
    fi
  done
}

# render_pngs WIDTH DESTDIR [transparent] -> SVG frames + PNG rasters (native at
# WIDTH). Pass "transparent" as the 3rd arg to drop the background (alpha PNGs).
render_pngs() {
  local w="$1" dest="$2" mode="${3:-opaque}" h
  h="$(awk "BEGIN { printf \"%d\", ${w} * 100 / ${ASPECT} + 0.5 }")"
  if [[ "${mode}" == "transparent" ]]; then
    python3 "${GEN}" --transparent "${dest}" >/dev/null
  else
    python3 "${GEN}" "${dest}" >/dev/null
  fi
  local svg
  for svg in "${dest}"/f*.svg; do
    resvg --width "${w}" --height "${h}" "${svg}" "${svg%.svg}.png"
  done
}

main() {
  require python3 resvg magick img2webp ffmpeg
  work="$(mktemp -d)"

  # Committed asset: light TRANSPARENT WebP, native 480 px, all 64 frames
  # (smooth). img2webp -m6 packs the alpha frames ~15% smaller than magick, and
  # q74 is visually identical to q80 on the soft plume.
  echo "rendering light transparent WebP -> ${WEBP}"
  render_pngs 480 "${work}/w480" transparent
  img2webp -loop 0 -d 40 -lossy -q 74 -m 6 "${work}/w480"/f0*.png -o "${WEBP}" >/dev/null

  # Social video: full fidelity, cropped from the 1.91:1 frame to ~4:3 (drops the
  # side margins for a feed upload); loops the 64-frame cycle three times.
  mkdir -p "${OUTDIR}"
  echo "rendering social MP4 -> ${OUTDIR}/wormhole-animation.mp4"
  render_pngs 1200 "${work}/w1200"
  ffmpeg -y -loglevel error -stream_loop 2 -framerate 25 \
    -i "${work}/w1200/f%03d.png" \
    -vf "crop=840:628:184:0,scale=1080:808:flags=lanczos" \
    -c:v libx264 -profile:v high -pix_fmt yuv420p -crf 18 \
    -movflags +faststart -an "${OUTDIR}/wormhole-animation.mp4"

  echo "done:"
  echo "  $(du -h "${WEBP}" | cut -f1)  ${WEBP}  (committed)"
  echo "  $(du -h "${OUTDIR}/wormhole-animation.mp4" | cut -f1)  wormhole-animation.mp4  (social)"
}

main "$@"
