#!/usr/bin/env bash
#
# Render the animated wormhole logomark to GIF / WebP / MP4.
#
# Pipeline: generate-wormhole-animation.py emits per-frame SVGs; resvg rasters
# them natively; ImageMagick packs the looping GIF/WebP; ffmpeg encodes the MP4.
# The outputs are generated artefacts and are NOT committed (see README.md).
#
# Deps: python3, resvg, ImageMagick 7 (`magick`), ffmpeg.
#       brew install resvg imagemagick ffmpeg
#
# Usage: ./render-wormhole-animation.sh [OUTDIR]   # OUTDIR defaults to ./wormhole-out
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly GEN="${SCRIPT_DIR}/generate-wormhole-animation.py"
readonly OUTDIR="${1:-${PWD}/wormhole-out}"
readonly W=1200 H=630          # social-card raster size (1.91:1)
readonly FPS=25                # matches the GIF's 4-centisecond frame delay

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

# render_frames DIR [generator-flags...] -> SVG frames + their PNG rasters in DIR.
render_frames() {
  local dir="$1"; shift
  python3 "${GEN}" "$@" "${dir}" >/dev/null
  local svg
  for svg in "${dir}"/f*.svg; do
    resvg --width "${W}" --height "${H}" "${svg}" "${svg%.svg}.png"
  done
}

main() {
  require python3 resvg magick ffmpeg

  work="$(mktemp -d)"
  mkdir -p "${OUTDIR}"

  echo "rendering frames (opaque + transparent)..."
  render_frames "${work}/frames"
  render_frames "${work}/frames_t" --transparent

  echo "encoding GIF (opaque, 800px wide)..."
  magick -delay 4 -loop 0 "${work}/frames"/f0*.png \
    -resize 800x -colors 128 -layers Optimize "${OUTDIR}/wormhole-animation.gif"

  echo "encoding WebP (transparent, ${W}px wide)..."
  magick -dispose Background -delay 4 -loop 0 "${work}/frames_t"/f0*.png \
    -background none -define webp:lossless=false -quality 86 \
    "${OUTDIR}/wormhole-animation.webp"

  # Crop the 1.91:1 frame to the ~4:3 content region (gate + plume) for a tight
  # social-video upload; loop the 64-frame cycle three times for a longer play.
  echo "encoding MP4 (opaque, cropped 4:3 for social upload)..."
  ffmpeg -y -loglevel error -stream_loop 2 -framerate "${FPS}" \
    -i "${work}/frames/f%03d.png" \
    -vf "crop=840:628:184:0,scale=1080:808:flags=lanczos" \
    -c:v libx264 -profile:v high -pix_fmt yuv420p -crf 18 \
    -movflags +faststart -an "${OUTDIR}/wormhole-animation.mp4"

  echo "done -> ${OUTDIR}"
  local f
  for f in gif webp mp4; do
    echo "  $(du -h "${OUTDIR}/wormhole-animation.${f}" | cut -f1)  wormhole-animation.${f}"
  done
}

main "$@"
