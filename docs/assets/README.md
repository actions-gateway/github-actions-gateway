# Repository assets

## Logomark & icons

The brand mark is the **four-tier motif** (Gateway Manager → Controller → Egress
Proxy → Worker Pod) as a tapering stack, in the Kubernetes-blue → teal gradient
(`#326CE5 → #2DD4BF`) — the same identity as the social card.

Edit the **SVG masters**; never hand-edit a generated raster.

| Master (edit this) | Generated output(s) | Used for |
| --- | --- | --- |
| `logo.svg` | — (SVG used directly) | header logomark, via `theme.logo` |
| `favicon.svg` | `favicon-16.png`, `favicon-32.png`, `favicon-48.png`, `favicon.ico` | browser-tab favicon (the SVG itself is wired via `theme.favicon`; the `.ico` is the raster fallback) |
| `icon-tile.svg` | `apple-touch-icon.png` (180), `icon-512.png` | iOS / PWA icons (opaque navy tile) |

`favicon.svg` is the rounded tile; `icon-tile.svg` is the full-bleed tile with
maskable-safe padding. The Apple/PWA rasters and the `.ico` are linked from
`overrides/main.html`.

### Re-rendering the rasters

Generated with [resvg](https://github.com/linebender/resvg) — a single static
binary, no browser. Install with `brew install resvg`. Render natively at each
target size (do **not** render large and downscale — the thin bars soften under a
resample). Run from `docs/assets/`:

```sh
# Transparent-tile favicons + packed .ico
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

Keep the PNG in sync with the SVG. Render at exactly 1280×640 (the size the SVG
declares) with any SVG renderer. With headless Chrome:

```sh
cd docs/assets
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless --disable-gpu --force-device-scale-factor=1 \
  --window-size=1280,640 --hide-scrollbars --default-background-color=00000000 \
  --screenshot=social-preview.png "file://$PWD/social-preview.svg"
```

Equivalent alternatives: `rsvg-convert -w 1280 -h 640 social-preview.svg -o social-preview.png`,
Inkscape, or macOS Quick Look (`qlmanage -t -s 1280 -o . social-preview.svg`).
