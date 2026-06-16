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
[`docs/assets/README.md`](../assets/README.md).

## Progressive enhancement (`docs/javascripts/extra.js`)

The interactive features layer on top of plain markdown that already renders on
github.com, so they must degrade to readable content without JS:

| Feature | Source markdown it enhances |
|---|---|
| Persona filter chips + per-row pills | the `Personas` column of the table in `docs/operations/README.md` |
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
