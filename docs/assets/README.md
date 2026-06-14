# Repository assets

## Logomark & icons

The brand mark is the **four-tier motif** (Gateway Manager → Controller → Egress
Proxy → Worker Pod) as a tapering stack, in the Kubernetes-blue → teal gradient
(`#326CE5 → #2DD4BF`) — the same identity as the social card.

- **`logo.svg`** — header logomark (transparent, gradient bars). Wired via
  `theme.logo` in `mkdocs.yml`.
- **`favicon.svg`** — browser-tab favicon (rounded navy tile). Wired via
  `theme.favicon`.
- **`icon.svg`** — editable master for the raster app icons (full-bleed navy,
  `width/height="100%"` so it scales to the render window). **Edit this**, then
  re-render the PNGs below.
- **`apple-touch-icon.png`** (180×180), **`icon-192.png`**, **`icon-512.png`** —
  rasters linked from `<head>` via `overrides/main.html`.

### Re-rendering the icon PNGs

Keep them in sync with `icon.svg`. With headless Chrome (one per size):

```sh
cd docs/assets
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
for s in apple-touch-icon:180 icon-192:192 icon-512:512; do
  "$CHROME" --headless --disable-gpu --force-device-scale-factor=1 \
    --window-size="${s##*:},${s##*:}" --hide-scrollbars \
    --screenshot="${s%%:*}.png" "file://$PWD/icon.svg"
done
```

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
