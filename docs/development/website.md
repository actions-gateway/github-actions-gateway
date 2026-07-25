# Documentation website

The public documentation + marketing site, served at the custom apex domain
[`actions-gateway.com`](https://actions-gateway.com/) and built from the `docs/`
tree with [MkDocs Material](https://squidfunk.github.io/mkdocs-material/).

- Config: `mkdocs.yml` · theme overrides: `overrides/` · styles + scripts:
  `docs/stylesheets/extra.css`, `docs/javascripts/extra.js`
- Deployed by `.github/workflows/pages.yml` as a **versioned tree** (see
  [§ Versioned deploy](#versioned-deploy-mike)): the site tracks the latest
  **stable release**, with the unreleased `main` docs available as an opt-in `dev`
  version. Pull requests only build/validate (never publish).

## Custom domain

The site is served from the apex domain **`actions-gateway.com`** (purchased
2026-06; replaced the original `actions-gateway.github.io/github-actions-gateway/`
project-page subpath). Two pieces keep the domain bound to the Actions-based
Pages deploy:

- **`docs/CNAME`** — contains the bare domain `actions-gateway.com`. MkDocs copies
  `docs_dir` root files verbatim into each built version, and the `pages.yml`
  `publish` job **re-asserts it at the artifact root** (`mike` does not keep a root
  `CNAME`). Without a root CNAME, an Actions deploy would clear the custom domain.
  Don't delete or rename it.
- **`site_url: https://actions-gateway.com/`** in `mkdocs.yml` — drives canonical
  URLs, `sitemap.xml`, and Open Graph / social meta, and roots the site at `/`
  (no more `/github-actions-gateway/` base path).

DNS (managed at the registrar): four apex `A` records → GitHub Pages
`185.199.108–111.153`, matching `AAAA` records, and a `www` `CNAME` →
`actions-gateway.github.io`. The repo Pages custom domain is set server-side
(`gh api -X PUT repos/{owner}/{repo}/pages -f cname=actions-gateway.com`), which
provisions a Let's Encrypt cert; "Enforce HTTPS" is enabled once the cert reads
`approved`. **DNSSEC and org-level domain verification remain optional future
hardening — not yet done.**

(The original build plan and decision log is `docs/plan/website.md`; this doc is
the durable how-to-maintain reference.)

## Versioned deploy (mike)

The published site is a **versioned tree** managed by
[`mike`](https://github.com/jimporter/mike) (Q238), not a single copy of `main`.
Each version is a full built copy under its own path on the `gh-pages` branch, and
Material renders a **version selector** from `mike`'s `versions.json`
(`extra.version.provider: mike` in `mkdocs.yml`):

| Version | Source | Selector entry | Indexed |
|---|---|---|---|
| `stable` (alias of the latest release, e.g. `1.1.0`) | the stable `vX.Y.Z` tag | default — root `/` redirects here | yes |
| older releases (`1.0.0`, …) | each stable `vX.Y.Z` tag | listed, reachable via the selector | yes |
| `dev` | the `main` branch | opt-in via the selector, titled "dev (main)" | no (`robots.txt` disallows `/dev/`) |

`stable` is the default, so a visitor lands on the latest **released** docs — a
feature merged to `main` but not yet in a tagged/released chart appears only under
the opt-in `dev` version, never as "Available now" on the released site.

**What deploys when** (`.github/workflows/pages.yml`):

- **push to `main`** → `mike deploy dev` (refreshes the unreleased `dev` docs).
- **stable `v*` tag push** → deploys that release's docs, and **only if it is the
  highest released version** also moves the `stable` alias + the default root
  redirect to it (`mike set-default stable`). This is the same tag push that runs
  `publish.yml`.
- **prerelease tag** (`0.x`, `-rc`/`-alpha`/`-beta`) → **no deploy** (the same
  prerelease test `publish.yml` uses, Q293).
- **`workflow_dispatch`** → deploys the `version`/`alias` inputs verbatim (or
  derives from the ref when blank) — used for **seeding** already-released versions
  and manual redeploys. The `docs_ref` input picks which ref the docs *content* is
  built from, independently of the ref the workflow logic runs from (see § Seeding
  below).

**Backports don't demote the site.** A patch cut for an older supported line (e.g.
`v1.2.5` released *after* `v1.3.0`) publishes/updates its own `1.2.5` version but
leaves `stable` on `1.3.0` — the deploy claims `stable` only when the pushed tag is
the highest released version. That backport must be tagged off the release line,
not off feature-ahead `main`; see
[release.md § Patch releases and backports](../operations/release.md#patch-releases-and-backports).

Pages source stays **"GitHub Actions"**: `mike` maintains the tree on `gh-pages`,
and the `publish` job serves that whole tree as the Pages artifact.
`--alias-type=copy` makes `stable/` a real directory — GitHub Pages artifact
deploys don't follow symlinks, so a symlinked alias would 404 on deep links.

### Seeding already-released versions

`mike` only knows the versions it has deployed, so releases cut **before** this
workflow landed aren't in the tree yet, and **the site root has nothing to redirect
to until some version claims the default**: the apex domain 404s while every push
still reports success. That was the state from the Q238 cutover until the first
seed, because `mike set-default` runs only for a stable tag push or an explicit
dispatch, and the last release predated the cutover by four days. The
*Verify the artifact serves a reachable root* step now fails the deploy rather than
publishing a 404 silently.

Seed the existing releases once via `workflow_dispatch`, **oldest first, newest
last** so `stable` ends on the latest:

| `version` | `docs_ref` | `alias` | `set_default` |
|---|---|---|---|
| `1.0.0` | `v1.0.0` | *(blank)* | `true` |
| `1.1.0` | `v1.1.0` | *(blank)* | `true` |
| `1.2.0` | `v1.2.0` | `stable` | `true` |

Run each from `main`, waiting for one to finish before starting the next (they
serialise on the shared `pages-deploy` concurrency group):

```bash
gh workflow run pages.yml --ref main -f version=1.0.0 -f docs_ref=v1.0.0 -f set_default=true
```

`set_default=true` on **every** seed, not just the last, is deliberate. Each run
re-points the root redirect at the version it just deployed, so the root is
serviceable from the first seed onward and the final row leaves it on `stable`. With
`set_default` left off, the first two runs would produce a tree with no root
redirect at all, which the *Verify the artifact serves a reachable root* step
correctly rejects: their content would reach `gh-pages` but never get published.

**Always dispatch from `main`, and always set `docs_ref`.** The two are easy to get
backwards:

- *Dispatching from the tag doesn't work.* `workflow_dispatch` reads the workflow
  file **at the ref you dispatch on**, and a pre-versioning tag's `pages.yml` has
  neither these inputs nor `mike`. The run would reject the inputs, and its flat
  single-copy deploy would clobber the whole version tree.
- *Omitting `docs_ref` publishes the wrong content.* `mike` builds the **current
  checkout**, so a seed dispatched from `main` without it would publish
  feature-ahead `main` as the released docs, reintroducing the "Available now"
  drift that Q387 and versioning exist to prevent.

`docs_ref` restores that ref's `docs/` and `mkdocs.yml` over the working tree. Four
things are deliberately **not** taken from the tag:

| Kept from the current checkout | Why |
|---|---|
| `requirements-docs.txt` | An old pin predates `mike` and version-selector support. |
| `site_url` and `docs/CNAME` | Where the site lives is a property of the site, not of the release. `v1.0.0` predates the custom domain: it has no `docs/CNAME`, and its `site_url` still points at the retired `actions-gateway.github.io/github-actions-gateway/` subpath, which would give that version dead canonical URLs, sitemap, and announce-bar links. |
| `overrides/` | Site chrome, not release documentation. The announce bar advertises the newest release site-wide, which is the signal a visitor reading older docs wants. Taking it from the tag pins each version to whatever the banner said the day it was cut, and the banner has lagged: `v1.1.0` and `v1.2.0` both shipped saying *"v1.0.0 is here"*. |
| `extra.version` | A pre-versioning tag has no version block, so its pages would render with no selector and strand a visitor on an old release. |

**That safety net covers seeds only.** A stable tag push has a blank `docs_ref` and
builds the tag wholesale, `overrides/` included, so the announce bar must still be
correct **in the tag**. Bumping it is a pre-flight step in
[release.md](../operations/release.md#1-pre-flight).

The `mkdocs.yml` overrides ride in an
[`INHERIT`](https://www.mkdocs.org/user-guide/configuration/#inheritance) overlay
(`mkdocs.versioned.yml`) rather than a YAML rewrite, so the tag's own nav and config
stay byte-for-byte intact: MkDocs deep-merges the inherited mapping, and scalars
such as `site_url` are replaced.

A subsequent `main` push adds the `dev` entry. From then on the workflow maintains
everything automatically.

### Local preview of the versioned site

`make docs-serve` previews the **current working tree** only (no selector) — the
right tool for writing content. To preview the full versioned tree with the
selector, run `mike` from the docs venv against a local `gh-pages`:
`.venv-docs/bin/mike serve`.

## SEO & analytics

Three pieces of machine-readable/operational metadata are wired centrally so they
apply site-wide, not per page:

- **JSON-LD structured data** — `overrides/main.html`'s `extrahead` block emits
  `SoftwareSourceCode` and `Organization` schema on every page, populated from
  `mkdocs.yml` (`site_name`, `site_description`, `site_url`, `repo_url`). Editing
  those config values updates the structured data automatically; don't hand-paste
  schema into individual pages. Validate built output at
  [validator.schema.org](https://validator.schema.org/).
- **`robots.txt`** — `docs/robots.txt` lands inside each built version; the
  `pages.yml` `publish` job writes the **root** `robots.txt` that points crawlers
  at the default (stable) `sitemap.xml` and `Disallow`s `/dev/` so the unreleased
  version stays out of the index (avoiding duplicate content across versions).
- **`sitemap.xml`** — generated automatically by MkDocs Material because
  `site_url` is set; no extra config. `robots.txt` points crawlers at it.

### Analytics (Plausible — opt-in)

Privacy-respecting analytics (Plausible: no cookies, no Google Analytics) are
wired via config in `mkdocs.yml` under `extra.analytics` and rendered by
`overrides/main.html`. **Disabled by default** — the script is only emitted when
`plausible_domain` is non-empty:

```yaml
extra:
  analytics:
    plausible_domain: ""                              # set to enable, e.g. actions-gateway.com
    plausible_src: https://plausible.io/js/script.js  # override for a self-hosted instance
```

To turn analytics on, a maintainer sets `plausible_domain` to the public site
domain (this is **not** a secret — it is the same `actions-gateway.com` already
in `site_url`) and registers that domain in their Plausible dashboard. Point
`plausible_src` at a self-hosted Plausible to avoid the hosted `plausible.io`.

## Fonts

The site's typefaces are **self-hosted** — no font CDN is contacted. Material's
built-in Google Fonts loader would otherwise fetch Roboto from
`fonts.gstatic.com` on every page view, leaking each visitor's IP and
User-Agent to Google (the one Google request an otherwise Google-free site
would still make). We disable it and serve our own woff2 instead:

- **`theme.font: false`** in `mkdocs.yml` turns off the loader.
- **`@font-face` declarations + the `--md-text-font` / `--md-code-font` mapping**
  in `docs/stylesheets/extra.css` point Material at the vendored files. Material
  appends its own system fallback stack, so a face that fails to load degrades to
  the OS font rather than a serif.
- **woff2 files** live in `docs/assets/fonts/` (latin subset, SIL OFL 1.1). See
  that directory's `README.md` for the file→role table, licensing, and the
  re-fetch commands.

The pairing is "GitHub-native": **Monaspace Neon** (GitHub's own superfamily) as
the display face on the landing hero and each page's `h1`, **IBM Plex Sans** for
body copy and headings `h2`–`h6`, and **Monaspace Argon** for code. To change a
face, add its weights to `docs/assets/fonts/`, add matching `@font-face` rules,
and update the `--md-*-font` variables (and the `h1` override for the display
face). Keep every face self-hosted — never reintroduce a `theme.font` mapping or
an external font `<link>`, which would restore the Google request.

### No flash-of-unstyled-text (FOUT)

Self-hosting alone would still "pop" — the browser only requests a font after
parsing CSS, so it paints fallback text, then swaps and reflows once the real
face arrives. Two coordinated pieces stop that:

- **`font-display: optional`** on the text and display faces (`extra.css`). Unlike
  `swap` (zero block period → always paints fallback first), `optional` gives the
  font a brief window to arrive before first paint and never swaps *late* — so
  there's no reflow either way. The code face (Monaspace Argon) stays `swap`: it's
  below the fold and we'd rather it always end up in real Monaspace than be dropped
  to the system monospace on a slow first load.
- **`<link rel="preload">`** for the three above-the-fold faces (Plex Sans Regular
  + Bold and Monaspace Neon Medium) in `overrides/main.html`, using the `| url`
  filter so the path is correct in both local serve and production, and
  `crossorigin` because fonts are always fetched in CORS mode. This starts the
  fetch during head parse, so those faces are in memory before first paint.

Net effect (verified in the preview via the Performance API): the preloaded faces
finish loading ~30 ms in, well before first contentful paint, so text renders in
the correct font on the first frame — no pop. If you add a weight that appears
above the fold, preload it too, or it may briefly render in the fallback.

## Local preview

```sh
make docs-serve   # live-reload preview at http://localhost:8000/
make docs-build   # one-shot static build into site/
```

Both targets provision an **isolated venv** (`.venv-docs/`, gitignored) from the
pinned `requirements-docs.txt` and reuse it across runs, so the docs toolchain
never touches the host Python — `python3` is the only host prerequisite
(`scripts/check-tools.sh`, extended tier). On Debian/Ubuntu the stdlib venv
module ships separately: `apt-get install python3-venv` if `python3 -m venv`
fails.

The toolchain is pinned **exactly** — MkDocs 2.0 is incompatible with Material 9.x,
so don't float the versions in `requirements-docs.txt`.

## Publication scope

`mkdocs.yml`'s `exclude_docs` keeps repo-internal trees off the public site:
`docs/plan/`, `docs/STATUS.md`, and `docs/development/` (this file included) are
GitHub-only. Published: `docs/design/`, `docs/operations/`, the landing page
(`docs/index.md`), and the `why-gag.md` comparison.

## Brand assets

The logomark and icon set are **generated, not hand-edited**. Edit
`docs/assets/generate-logomark.py` (the parametric faceted-ring mark) and
re-render the rasters with resvg — full procedure in
[`docs/assets/README.md`](../assets/README.md). The same README also covers the
animated wormhole logomark (`generate-wormhole-animation.py` +
`render-wormhole-animation.sh`); the light looping WebP is committed (README
footer + 404 page) and the full-fidelity MP4 is generated on demand into the
gitignored `tmp/`.

## Progressive enhancement (`docs/javascripts/extra.js`)

The interactive features layer on top of plain markdown that already renders on
github.com, so they must degrade to readable content without JS:

| Feature | Source markdown it enhances |
|---|---|
| Persona filter chips + per-row pills (clicking a row pill selects its chip) | the `Personas` column of the table in `docs/operations/README.md` |
| Per-doc audience pills | the `> **Audience:** …` blockquote under each operations doc's title |
| Reading-path role chips | the bold role leads (`**Architect**`, …) in `docs/design/README.md` § Reading Paths by Role |
| Scroll reveals | landing + `why-gag` pages only (skipped for `prefers-reduced-motion` / no-JS) |

**Keep those source markers intact** when editing — deleting the table column, a
blockquote, or a bold role lead silently breaks the matching site feature.

## Persona / audience tags live in two places

A doc's audience is recorded twice, by design:

1. the operations index `Personas` column (`docs/operations/README.md`) — drives
   the filter chips, and
2. that doc's own `> **Audience:** …` blockquote — drives the per-doc pill.

When you retag a doc, **update both**; they should agree. There is no CI check —
it's two lines kept in sync by hand (deliberately not worth automating).

The per-doc pills also **deep-link** to `operations/?persona=<persona>`, and the
index reads that query param on load to pre-apply the matching chip. The link is
generated from the blockquote, so keeping (1) and (2) in agreement is enough —
just don't rename a persona in only one place.
