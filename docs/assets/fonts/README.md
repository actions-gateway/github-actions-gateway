# Self-hosted webfonts

These woff2 files back the docs site's "GitHub-native" type pairing.
They are served from our own origin so the site makes **no third-party font requests** — Material's Google Fonts loader is disabled via `theme.font: false` in [`mkdocs.yml`](../../../mkdocs.yml), and the `@font-face` + `--md-text-font`/ `--md-code-font` wiring lives in [`docs/stylesheets/extra.css`](../../stylesheets/extra.css).
Background: [`docs/development/website.md`](../../development/website.md) § Fonts.

| File | Family / role | Weight | Source |
|---|---|---|---|
| `IBMPlexSans-Regular.woff2`  | IBM Plex Sans — body/UI  | 400        | [@fontsource/ibm-plex-sans](https://fontsource.org/fonts/ibm-plex-sans) (latin subset) |
| `IBMPlexSans-Italic.woff2`   | IBM Plex Sans — emphasis | 400 italic | same |
| `IBMPlexSans-SemiBold.woff2` | IBM Plex Sans — strong   | 600        | same |
| `IBMPlexSans-Bold.woff2`     | IBM Plex Sans — h2–h6    | 700        | same |
| `MonaspaceNeon-Medium.woff2` | Monaspace Neon — display (hero + h1) | 500 | [githubnext/monaspace](https://github.com/githubnext/monaspace) v1.101 |
| `MonaspaceNeon-Bold.woff2`   | Monaspace Neon — display bold        | 700 | same |
| `MonaspaceArgon-Regular.woff2` | Monaspace Argon — code             | 400 | same |

## Licensing

Both families are licensed under the **SIL Open Font License 1.1**, which permits bundling and redistributing them with software (commercially too), provided the fonts aren't sold on their own and each family's copyright notice + license text travel with them.
Each family keeps its **own** license file — copy the matching one wherever these fonts go:

- **IBM Plex Sans** — Copyright 2019 IBM Corp. No Reserved Font Name (IBM chose not to reserve "Plex", so the name may be used freely, subsets included).
  License: [`IBMPlexSans-OFL.txt`](IBMPlexSans-OFL.txt).
- **Monaspace** (Neon, Argon) — Copyright © 2023 GitHub, with Reserved Font Name "Monaspace".
  Shipped here unmodified.
  License: [`Monaspace-OFL.txt`](Monaspace-OFL.txt).

Each `.woff2` also embeds its own copyright + license in its `name`-table metadata, so the notice travels with the binary even on its own.
The wider repo is Apache-2.0; these font files are governed by the OFL license files here, not by the repo license.

## Updating

Re-fetch the same weights (keep the filenames stable so the CSS keeps resolving):

```bash
FS="https://cdn.jsdelivr.net/npm/@fontsource"
MS="https://cdn.jsdelivr.net/gh/githubnext/monaspace@v1.101/fonts/webfonts"
curl -fsSL "$FS/ibm-plex-sans/files/ibm-plex-sans-latin-400-normal.woff2" -o IBMPlexSans-Regular.woff2
curl -fsSL "$FS/ibm-plex-sans/files/ibm-plex-sans-latin-400-italic.woff2" -o IBMPlexSans-Italic.woff2
curl -fsSL "$FS/ibm-plex-sans/files/ibm-plex-sans-latin-600-normal.woff2" -o IBMPlexSans-SemiBold.woff2
curl -fsSL "$FS/ibm-plex-sans/files/ibm-plex-sans-latin-700-normal.woff2" -o IBMPlexSans-Bold.woff2
curl -fsSL "$MS/MonaspaceNeon-Medium.woff2"   -o MonaspaceNeon-Medium.woff2
curl -fsSL "$MS/MonaspaceNeon-Bold.woff2"     -o MonaspaceNeon-Bold.woff2
curl -fsSL "$MS/MonaspaceArgon-Regular.woff2" -o MonaspaceArgon-Regular.woff2
```
