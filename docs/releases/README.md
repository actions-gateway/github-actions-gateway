# Release notes

One file per stable tag, `vX.Y.Z.md`, holding that release's **GitHub Release
body verbatim** — no front matter, no heading above the first line.

**No title heading**, specifically. The Releases page renders the tag name as the
page's own `<h1>`, so a `# vX.Y.Z` in the body is a duplicate title one line under
the real one. The file therefore opens on its first real sentence, and its
identity comes from the filename — which is also why stripping a heading at
publish time is not the answer: the published body has to stay byte-identical to
this file, or the drift these files exist to prevent comes back.

The file is the authoring source; the published body is a copy of it:

```bash
gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md
```

Curating a release body is a `publish.yml` step with its own method and its own
traps (what to promote, what the caveats script actually reports, why hard-wrapping
breaks the rendering). All of it lives in
[operations/release.md § Writing the curated notes](../operations/release.md#writing-the-curated-notes).

| Release | Notes |
|---|---|
| v1.3.0 | [v1.3.0.md](v1.3.0.md) |

Releases before `v1.3.0` predate this convention; their bodies live only on the
Releases page. Retrieve one with `gh release view vX.Y.Z --json body --jq .body`.

## Not published to the docs site

These files are written for github.com's **comment-flavour GFM** renderer, which
the MkDocs site is not. Two differences matter:

- GFM alerts (`> [!WARNING]`, `> [!CAUTION]`, `> [!NOTE]`) render on github.com and
  publish as literal text under MkDocs.
- A single newline inside a paragraph becomes `<br>` in a release body, so these
  files are deliberately **not hard-wrapped**. Do not reflow them. Verify against
  the renderer (`gh api -X POST /markdown -f mode=gfm …`), never against the raw
  Markdown — the source has no `<br>` in it either way.

`docs/releases/` is therefore excluded from every site version, `dev` included.
The exclusion is spelled out in four places that must agree — `mkdocs.yml`, two
`env:` blocks and one `export` in `.github/workflows/pages.yml`, and
`scripts/docs/docs-preview.sh`. Because the directory is never a site page, a doc
that links here must use the absolute `github.com` URL; a relative link fails
`mkdocs build --strict`.

Their own links point into the **versioned docs site**
(`https://actions-gateway.com/X.Y.Z/…`, no leading `v`), so an operator reading
the notes for a release gets that release's instructions. `make doc-links` skips
external URLs by design, so those links are not checked — verify them by hand.
