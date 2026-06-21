# Repository assets

## Logomark & icons

The brand mark is a **faceted "gateway ring"** — a crystalline torus (a portal
you pass through) in the Kubernetes-blue → teal gradient (`#326CE5 → #2DD4BF`),
lit from the top-left for depth. Same identity as the social card.

The three SVG masters are **generated** from `generate-logomark.py` (the ring is
parametric — point count, spike depth, contrast, and light angle are all tunable
constants at the top of the script). Run it from `docs/assets/`, then re-render
the rasters with resvg:

```sh
python3 generate-logomark.py   # writes logo.svg, icon-tile.svg, favicon.svg
```

| Master (generated) | Raster output(s) | Used for |
| --- | --- | --- |
| `logo.svg` | — (SVG used directly) | header logomark, via `theme.logo` |
| `favicon.svg` | `favicon-16.png`, `favicon-32.png`, `favicon-48.png`, `favicon.ico` | browser-tab favicon — a **simplified** spiky ring (the star silhouette, no internal facet seams) that stays legible at 16 px (the SVG is wired via `theme.favicon`; the `.ico` is the raster fallback) |
| `icon-tile.svg` | `apple-touch-icon.png` (180), `icon-512.png` | iOS / PWA icons (full faceted ring on an opaque navy tile, maskable-safe padding) |

The Apple/PWA rasters and the `.ico` are linked from `overrides/main.html`. The
social card (`social-preview.svg`, below) has the ring baked in inline with its
kicker — re-run `generate-logomark.py`'s `social_ring_group()` and re-paste if
the ring geometry changes.

### Re-rendering the rasters

Generated with [resvg](https://github.com/linebender/resvg) — a single static
binary, no browser. Install with `brew install resvg`. Render natively at each
target size (do **not** render large and downscale — facets soften under a
resample). Run from `docs/assets/`:

```sh
# Simplified-ring favicons + packed .ico
for s in 16 32 48; do resvg -w $s -h $s favicon.svg favicon-$s.png; done

# Opaque tile icons (iOS / PWA), rendered natively at the target size
resvg -w 180 -h 180 icon-tile.svg apple-touch-icon.png
resvg -w 512 -h 512 icon-tile.svg icon-512.png

# Pack favicon.ico (PNG-in-ICO; supported by all modern browsers)
python3 - <<'PY'
import struct
sizes = [16, 32, 48]
pngs = [(s, open(f"favicon-{s}.png", "rb").read()) for s in sizes]
n = len(pngs); header = struct.pack("<HHH", 0, 1, n)
entries, off = b"", 6 + 16 * n
for s, d in pngs:
    w = h = (0 if s >= 256 else s)
    entries += struct.pack("<BBBBHHII", w, h, 0, 0, 1, 32, len(d), off); off += len(d)
with open("favicon.ico", "wb") as f:
    f.write(header); f.write(entries)
    for _, d in pngs: f.write(d)
print("packed favicon.ico")
PY
```

Verify: `file favicon.ico` should report three icons (16/32/48).

## Social preview card — `social-preview.svg` / `social-preview.png`

The GitHub social preview (Open Graph) image for this repository: the card shown
when the repo link is shared on Slack, X, LinkedIn, and other link unfurlers.

- **`social-preview.svg`** — the editable source. Make changes here.
- **`social-preview.png`** — a 1280×640 raster rendered from the SVG. This is the
  file uploaded to GitHub, because **Settings → General → Social preview** only
  accepts a raster image (PNG/JPG/GIF), not SVG.

GitHub does **not** read the social preview from the repository tree — it must be
uploaded manually via **Settings → General → Social preview**. Re-upload
`social-preview.png` whenever it changes.

### Re-rendering the PNG

Rendered with [resvg](https://github.com/linebender/resvg) (`brew install resvg`)
— the same renderer as the icons above. The SVG declares its 1280×640 size, so no
`-w/-h` is needed. It uses CSS system-font stacks (`-apple-system`, …) which are
**not** real font names, so pass concrete installed families or the metrics shift.
Run from `docs/assets/`:

```sh
resvg --sans-serif-family "Helvetica Neue" --monospace-family "Menlo" \
  social-preview.svg social-preview.png
```

Verify the result has no clipped text and a crisp logomark.

## Animated logomark

A looping animation of the gateway ring as a literal Stargate-style wormhole.
Like the static mark it is **generated, not hand-authored**:
`generate-wormhole-animation.py` emits the per-frame SVGs and
`render-wormhole-animation.sh` rasters and packs them.

One ~2.6 s loop: a closed crystalline spiral iris opens, the wormhole ignites and erupts a
water/plasma "kawoosh" along the gate's normal, the plume expands and retracts
into a shimmering event horizon, and the iris closes again — seamless at the loop
boundary. The ring geometry, palette, timing, and iris are tunable constants at
the top of the Python file.

```sh
# needs resvg, ImageMagick 7 (magick), ffmpeg, python3
#   brew install resvg imagemagick ffmpeg
./render-wormhole-animation.sh [OUTDIR]   # MP4 OUTDIR defaults to <repo>/tmp/wormhole/
```

| Artefact | Committed? | Format | Used for |
| --- | --- | --- | --- |
| `wormhole-animation.webp` | **yes** (~325 KB) | 480 px, opaque, looping | README footer + the docs **404** page (`overrides/404.html`) |
| `wormhole-animation.mp4` | no — written to `OUTDIR` (default `tmp/wormhole/`, gitignored) | 1080×808 (~4:3), opaque | social upload, e.g. Bluesky/X (animates where an uploaded GIF/WebP would be static) |

The committed WebP is **opaque** (it carries the dark navy backdrop) on purpose:
the plume and glow are white, and both the README and the docs site default to
light mode, so a transparent version would wash out. It is kept small (480 px,
quality 72) so it doesn't weigh down page loads. The MP4 is full-fidelity and
cropped tighter than the 1.91:1 WebP to drop the side margins for a feed video.
