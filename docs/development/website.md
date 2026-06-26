# Documentation website

The public documentation + marketing site, served at the custom apex domain
[`actions-gateway.com`](https://actions-gateway.com/) and built from the `docs/`
tree with [MkDocs Material](https://squidfunk.github.io/mkdocs-material/).

- Config: `mkdocs.yml` · theme overrides: `overrides/` · styles + scripts:
  `docs/stylesheets/extra.css`, `docs/javascripts/extra.js`
- Deployed by `.github/workflows/pages.yml` — pushing docs changes to `main`
  publishes automatically; `workflow_dispatch` is available for a manual
  redeploy. Pull requests only build/validate (never publish).

## Custom domain

The site is served from the apex domain **`actions-gateway.com`** (purchased
2026-06; replaced the original `actions-gateway.github.io/github-actions-gateway/`
project-page subpath). Two pieces keep the domain bound to the Actions-based
Pages deploy:

- **`docs/CNAME`** — contains the bare domain `actions-gateway.com`. MkDocs copies
  `docs_dir` root files verbatim into the built site root, so every Pages artifact
  re-asserts the domain. Without it, an Actions deploy would clear the custom
  domain. Don't delete or rename it.
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

## SEO & analytics

Three pieces of machine-readable/operational metadata are wired centrally so they
apply site-wide, not per page:

- **JSON-LD structured data** — `overrides/main.html`'s `extrahead` block emits
  `SoftwareSourceCode` and `Organization` schema on every page, populated from
  `mkdocs.yml` (`site_name`, `site_description`, `site_url`, `repo_url`). Editing
  those config values updates the structured data automatically; don't hand-paste
  schema into individual pages. Validate built output at
  [validator.schema.org](https://validator.schema.org/).
- **`robots.txt`** — `docs/robots.txt` is copied verbatim into the site root (same
  mechanism as `docs/CNAME`). It allows all crawlers and references the sitemap.
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

## Local preview

```sh
pip install -r requirements-docs.txt   # pinned: mkdocs 1.6.1, mkdocs-material 9.7.6
mkdocs serve                           # http://localhost:8000/
```

The toolchain is pinned **exactly** — MkDocs 2.0 is incompatible with Material 9.x,
so don't float the versions.

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
