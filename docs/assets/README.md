# Repository assets

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
