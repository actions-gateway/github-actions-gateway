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

## The announce bar

The strip at the top of every page ("vX.Y.Z is here…") is the `announce` block in
[`overrides/main.html`](../../overrides/main.html). Its **version is derived, not
hand-written**: the MkDocs hook
[`hooks/release_version.py`](../../hooks/release_version.py) resolves it at build
time and exposes it to the template as `config.extra.release.version`.

| Source | When it applies |
|---|---|
| `$GAG_DOCS_RELEASE` | Set explicitly. An escape hatch for builds with no git history (a source tarball) and for exercising the template by hand. |
| Highest stable `vX.Y.Z` git tag | The normal path. "Stable" is the same test `publish.yml` and `pages.yml` share: a SemVer core of `0.x`, or any `-` suffix (`-rc`/`-alpha`/`-beta`), is a prerelease. |
| Nothing resolvable | The banner drops the version claim and links to the releases page, rather than guessing. |

It names the **newest** release, not the version being built, because the bar is
site chrome: a visitor reading older docs wants to know what the current release
is. That is also why `pages.yml` pins `overrides/` to the current checkout when
seeding (see [§ Seeding](#seeding-already-released-versions)).

The one hand-written part is the optional per-release **highlight** prose, and it
is guarded by a `highlight_for` version in the same template. The highlight renders
only while `highlight_for` names the resolved release, so forgetting to refresh it
degrades the bar to a release-notes link instead of pairing a new version with the
previous release's headline. Refreshing it is an optional pre-flight step in
[release.md](../operations/release.md#1-pre-flight).

Why this is derived at all: a stable tag deploys that tag's docs wholesale, so a
banner that is wrong at tag time is published permanently under that version and no
later fix to `main` reaches it. Every stable tag missed the manual bump: `v1.0.0`
shipped saying *"Alpha, pre-1.0"*, and `v1.1.0` and `v1.2.0` both said *"v1.0.0 is
here"* (Q393). `publish.yml`'s `announce-bar` gate now builds the site at the tag
and asserts the **rendered** bar names it, so a broken hook or template fails the
release before any image is pushed.

Both workflows check out with `fetch-depth: 0` for this reason: a default depth-1
checkout fetches no tags, and the bar would render the version-free fallback.
Locally, `make docs-serve` and `make docs-build` resolve tags from your checkout, so
run `git fetch --tags` if the bar looks stale. To see what the bar will name
without building the site:

```bash
python3 hooks/release_version.py
```

The tag-selection rules (numeric ordering, prerelease exclusion, the
`$GAG_DOCS_RELEASE` override, degrading to `""` outside a repository) are asserted
by `scripts/docs/release-version-hook-test.sh`, which `make check` runs via
`make scripts-test`.

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
| `overrides/` | Site chrome, not release documentation. The [announce bar](#the-announce-bar) advertises the newest release site-wide, which is the signal a visitor reading older docs wants. Taking it from the tag pins each version to the highlight prose written the day it was cut. |
| `extra.version` | A pre-versioning tag has no version block, so its pages would render with no selector and strand a visitor on an old release. |
| `hooks` | A tag cut before Q393 does not wire `hooks/release_version.py`, so its announce bar would render the version-free fallback instead of naming the newest release. |

**That safety net covers seeds only.** A stable tag push has a blank `docs_ref` and
builds the tag wholesale, `overrides/` included. That is safe for the announce
bar's *version*, which the hook derives from the tag list at build time rather than
from a string in the checkout; only the optional highlight prose is pinned to the
tag. Refreshing it is an optional pre-flight step in
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

## The stylesheet (`docs/stylesheets/extra.css`)

Every custom class is namespaced `gag-`, in two families:

- **Components** use BEM — `gag-hero`, `gag-hero__logo`, `gag-hero__phrase`. The
  block name matches the wrapper `<div>` in the Markdown.
- **Utilities** are bare and reusable — `gag-nowrap` (keeps a short code chip on
  one line inside a narrow table column), `gag-cont`.

**Grep the name before adding a class.** A collision does not error — it resolves
by specificity, silently. `.md-typeset .gag-nowrap` (0,2,0) beats a plain
`.gag-nowrap` (0,1,0), so a new rule reusing that name has no effect and the
symptom is "my CSS does nothing," with nothing in the build to explain it. A
utility that must never change behaviour and a component that must change it at a
breakpoint are different classes, even when the declaration is identical.

### Page-scoped table rules

Two pages pin their own table columns, each selected by an element only that page
has: `.md-content__inner:has(.gag-vs-hero)` for the why-GAG comparison, and
`.md-typeset:has(> h1#api-reference)` for the generated
[API reference](../reference/api.md). Both are there for the same reason. `auto`
table layout splits width by each column's max-content demand, so a column of
paragraphs takes width from the columns that carry the row's identity, and the
split moves as the container does. Pin the columns with `table-layout: fixed`
rather than tuning the prose.

The API reference block also restates the header background it overrides. The
global rules tint the **last** column, an idiom from the comparison table where
last means GAG, and they apply everywhere, because the reveal JS classes every
plain table (see [§ Progressive enhancement](#progressive-enhancement-docsjavascriptsextrajs)).
On the reference page the last column is Validation, so the tint reads as an
emphasis the page does not mean; dropping it means putting the ordinary `th`
background back, not just clearing the accent.

### Changing the hero headline

The headline's type size and its two breakpoints are **derived from the longest
unbreakable phrase**, not chosen for looks. The display face is monospace, so a
phrase's width is arithmetic: `characters × 0.61em`. `Self-hosted GitHub Actions`
is 26 characters — 15.86em, or 920px at the 2.9rem the headline used to cap at,
against a headline column of only 718px. That is why the cap is 2.2rem, why the
logomark stacks above the headline below 56rem instead of eating 162px of the
column, and why `gag-hero__phrase`'s `nowrap` releases below 44rem.

Measure before changing any of the three. Serve the site, then in the browser
console read the column against the phrase:

```js
const h = document.querySelector('.gag-hero h1');
h.clientWidth;                                    // column available
h.querySelector('.gag-hero__phrase').getBoundingClientRect().width;  // phrase needed
```

Sweep the viewport widths, not just your own — the binding case is ~1200px, where
the logomark hits its 132px size cap while the hero is already at its 44rem max.
Editing the headline **text** is subject to the same arithmetic: a phrase longer
than 26 characters needs the cap lowered again, or it will overrun the column.

## Local preview

```sh
make docs-serve   # live-reload preview at http://localhost:8000/
make docs-build   # strict build of both scopes into site/ and site-dev/
```

Both targets provision an **isolated venv** (`.venv-docs/`, gitignored) from the
pinned `requirements-docs.txt` and reuse it across runs, so the docs toolchain
never touches the host Python — `python3` is the only host prerequisite
(`scripts/ci/check-tools.sh`, extended tier). On Debian/Ubuntu the stdlib venv
module ships separately: `apt-get install python3-venv` if `python3 -m venv`
fails.

The toolchain is pinned **exactly** — MkDocs 2.0 is incompatible with Material 9.x,
so don't float the versions in `requirements-docs.txt`.

## The two link gates

`docs/` is rendered by two engines with different link semantics, so **one gate
cannot cover both** (Q560):

| Gate | Oracle | Runs in |
|---|---|---|
| `make doc-links` (`scripts/docs/check-doc-links.sh`) | github.com — GitHub's heading slugger, directory listings | `doc-links.yml`, `make check` |
| `make docs-build` (`mkdocs build --strict`) | the published site — Python-Markdown slugs, MkDocs path resolution | `pages.yml`'s PR `build` job |

The first gate reads the MkDocs dialect even though it answers for GitHub: its
parser handles `!!!` admonition bodies and `markdown="1"` HTML, so a link inside
one is checked rather than silently skipped as an indented code block (Q612).

Two engines, two gates. `docs/releases/` is the exception that needs a third:
those files publish to neither, and their links are all absolute into the
versioned site, so `make release-links-check` resolves them against a local
`site/` build ([testing.md § The release-link gate](testing.md#the-release-link-gate)).

A link can pass one and 404 on the other. Three divergences have actually shipped
broken:

- **Duplicate headings.** Both engines de-duplicate repeated heading slugs, with
  different suffixes: GitHub writes `#rollback-1`, Python-Markdown writes
  `#rollback_1`. Neither suffix is configurable, so **make the headings unique**
  rather than linking a generated suffix — four `### Rollback` sections became
  `### GMC rollback`, `### AGC rollback`, and so on. A duplicate heading is also
  how a repo-local TOC silently lies: two entries resolve to the same anchor and
  both gates pass.
- **Bare `dir/` targets.** GitHub renders `[x](examples/policies/)` as that
  directory's `README.md`; MkDocs has no directory to serve, leaves the link
  alone, and it resolves under the *page* URL. Link the `README.md` explicitly —
  MkDocs maps it to the section index, so both renderings work.
- **Angle brackets in a heading.** Python-Markdown strips `<field>` as an HTML
  tag before slugging; GitHub keeps the word and drops only the brackets. Spell
  placeholders without `<>`.

Only the PR gate is strict. The deploy job builds a tag's own docs, and a release
cut before this gate existed must stay publishable.

`mkdocs.yml`'s `validation` block is what makes this fail at all: MkDocs reports
these as INFO by default, invisible under a green build. The same block raises
`nav.omitted_files` for the nav-coverage gate
([§ What belongs in `nav`](#what-belongs-in-nav)); `absolute_links` and
`nav.not_found` keep their defaults deliberately, the latter because it already
defaults to `warn`.

Neither gate covers a link to a page the build's own scope excludes — MkDocs
clamps that one below warning level whatever `validation` says. That is
[`hooks/source_links.py`](../../hooks/source_links.py)'s job instead; see
[§ Unpublished is per build, not per path](#unpublished-is-per-build-not-per-path-q561).

## Publication scope

Scope is **per version**, not per site (Q558):

| Version | Publishes |
|---|---|
| `stable` and every numbered release | Operator docs only: `docs/design/`, `docs/operations/`, `docs/index.md`, `why-gag.md`, `features.md`, `roadmap.md` |
| `dev` | The above **plus** the repo-internal docs: `docs/STATUS.md` (the [backlog](#the-backlog-page)), `docs/plan/`, `docs/development/` (this file included), `docs/assets/`'s READMEs |
| *no version* | `docs/releases/` — see the trap below |

A release is a frozen build, so a backlog published in one would be a snapshot
stale from tag day. `dev` redeploys on every push to `main`, which is the only
place a live backlog is honest.

One env var carries the difference. `mkdocs.yml`'s `exclude_docs` is an
[`!ENV` tag](https://www.mkdocs.org/user-guide/configuration/#environment-variables)
whose **default** is the full repo-internal exclusion list; the `dev` deploy step
in `pages.yml` overrides `MKDOCS_EXCLUDE_DOCS` with a shorter one. Release
deploys never set it, so no release version — existing or future — can gain a
repo-internal page.

Two traps worth keeping in mind when editing that wiring:

- **Unset and empty mean opposite things.** The `!ENV` default applies only when
  the variable is *absent*. `pages.yml` therefore `export`s it inside a
  conditional rather than using a step-level `env:` with a
  `${{ … && … || '' }}` expression, which would set it to `''` on every release
  and publish the internal docs everywhere.
- **`docs/README.md` stays excluded on every version**, including `dev`. MkDocs
  drops it anyway as a conflict with the `index.md` landing page; leaving it in
  the list keeps that from surfacing as a build warning. Because it is never a
  site page, a doc that links to it must use the absolute `github.com` URL — a
  relative `../README.md` fails `mkdocs build --strict`.
- **`docs/releases/` is excluded on every version too**, and it is the one entry
  that is not a repo-internal-vs-operator split: those files are GitHub Release
  bodies, authored for github.com's comment-flavour renderer. Their GFM alerts
  (`> [!WARNING]`) have no MkDocs equivalent and would publish as literal text,
  and their links already point into the versioned site. The exclusion is spelled
  out in **four** places that must agree — `mkdocs.yml`'s default, the two `env:`
  blocks and the one `export` in `pages.yml`, and `scripts/docs/docs-preview.sh`.
  Miss the last and `make docs-build` disagrees with CI.

PR builds validate **both** scopes (`pages.yml`'s `build` job runs `mkdocs build`
twice), so a PR that breaks a plan or development page fails there rather than on
`main`. `make docs-build` does the same locally — see
[§ The two link gates](#the-two-link-gates).

A push to `main` re-runs that gate (`pages.yml`'s `validate` job), because a PR
gate only ever sees its own base. Two PRs open at once can each pass and still
merge into a red `main` — Q560 raised the link validation to warnings while Q558
added a link that trips it, and the breakage surfaced two hours later on an
unrelated docs PR (Q562). `validate` runs beside the deploy rather than gating
it: the point is a red status on `main`, not a stalled `dev` version.

### What belongs in `nav`

The `nav` is the operator-facing table of contents, so a published page is
either in it or declared in `not_in_nav`. Two kinds are declared:

- the repo-internal tree the `dev` version publishes (`STATUS.md`, `plan/`,
  `development/`), reachable by URL, by search, and from the roadmap's backlog
  link;
- `operations/examples/`, sample manifests reached from the page that explains
  them ([admission-policies.md](../operations/admission-policies.md)) rather
  than browsed from the TOC.

Everything else in `nav`. That makes MkDocs' "pages exist in the docs directory,
but are not included in the `nav` configuration" list the accidental omissions
only, and **the list must be empty** — an entry is a page a reader can now reach
only by search or URL. Q562 was three such pages (the admission-policy matrix,
Appendix H, the protocol-dependency register) sitting unnoticed in it.

**That is a gate, not a habit** (Q563). MkDocs reports the list at INFO, which
never fails a build, so `mkdocs.yml` raises `validation.nav.omitted_files` to
`warn` and `--strict` turns it into an error — the same INFO-to-warning move
[§ The two link gates](#the-two-link-gates) makes for links. Add a page and
`make docs-build` fails until it is either in `nav` or declared in `not_in_nav`:

```
The following pages exist in the docs directory, but are not included in the "nav" configuration:
  - operations/your-new-page.md
Aborted with 1 warnings in strict mode!
```

Both scopes are checked, because nav coverage is decided per build: a
`development/` page is a page only on `dev`, so the release-scope build cannot
see it. `not_in_nav` is what suppresses the report for a deliberate omission, so
it stays populated — the empty list is MkDocs' *reported* one, never this key.
The one inert entry is `/README.md`: MkDocs drops `docs/README.md` as an
`index.md` conflict before nav is evaluated, so it reaches no scope's list and is
kept only to mirror `exclude_docs`.

### The backlog page

`docs/STATUS.md` renders at
[`/dev/STATUS/`](https://actions-gateway.com/dev/STATUS/) with filter chips over
its tables — see [§ Progressive enhancement](#progressive-enhancement-docsjavascriptsextrajs).
The markdown file stays the single source of truth: the lint gate, the
isolated-commit merge discipline, the `<a id="QN">` anchors, and the
[backlog workflow](maintaining-backlog.md) all operate on the table, and the site
is a read-only view of it.

**How a reader reaches it.** The page is deliberately absent from the `nav` (see
[§ Publication scope](#publication-scope)), so three routes carry the traffic:

| Route | Reaches it from |
|---|---|
| The **version banner** (below) | any page of a non-default version — the only site-chrome entry point |
| [`roadmap.md`](../roadmap.md) | the intro paragraph and § How priorities are set, on **every** version (the URL is version-pinned, so it stays honest from `stable`, where the page does not exist) |
| [`docs/README.md`](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/README.md) | github.com, where contributors start |

Site search finds it too, but only *within* the `dev` version — Material's search
index is per-version, so a search run from `stable` will never surface it.

**Accepted wart:** mike's version switcher keeps the current path, so switching
from the backlog page to `stable` lands on a 404. That is inherent to any
per-version page.

### The version banner

The yellow bar reading *"You're not viewing the latest release"* is the
`outdated` block in [`overrides/main.html`](../../overrides/main.html). Material
renders that bar **only when a theme override fills the block** — the stock one
is empty — so before this existed no version carried the not-canonical framing at
all, `dev` included, despite `extra.version.default: stable` being set. Do not
assume the version selector implies a banner; it does not.

`stable` is the default, so every other version shows it: the unreleased `dev`
docs and any older release. The backlog link inside it is gated on
`config.extra.backlog_page`, which
[`hooks/backlog_link.py`](../../hooks/backlog_link.py) derives from the build's
own file set rather than from a second flag that could drift out of step with
`exclude_docs` — the link renders exactly where the page exists. An older release
therefore gets the warning without a link that would 404.

Two traps if you edit that block:

- **`exclude_docs` does not remove a file.** MkDocs keeps it and marks it
  `InclusionLevel.EXCLUDED`, so presence in `files` proves nothing — the hook has
  to read `file.inclusion.is_included()`. Testing only the former puts the link
  on every release.
- **`404.html` renders this block with no `page` in context.** Anything reading
  `page.url` there fails the whole build (and hand-building `{{ base_url }}/…`
  yields a protocol-relative `//STATUS/`). Hence the absolute `site_url` for the
  release link and the `| url` filter for the per-page one.

## Links into the source tree

`docs/` doubles as in-repo documentation browsed on github.com, where a relative
link like `../cmd/agc/main.go` resolves to the source file. MkDocs has no such
file to serve, so it leaves the link alone and the published page 404s.
[`hooks/source_links.py`](../../hooks/source_links.py) rewrites every relative
target **this build does not publish** into an absolute URL under `repo_url`, so
one markdown link works in both places:

| Written in markdown | Published as |
|---|---|
| `../cmd/agc/main.go` | `{repo_url}/blob/{ref}/cmd/agc/main.go` |
| `../cmd/agc/main.go:91` | `{repo_url}/blob/{ref}/cmd/agc/main.go#L91` |
| `../cmd/gmc/internal/` | `{repo_url}/tree/{ref}/cmd/gmc/internal` |

`{ref}` is `$GAG_DOCS_SOURCE_REF`, defaulting to `main`; `pages.yml` sets it to
the tag when deploying a release, so each version links to the source it
documents. A target that **doesn't exist in the working tree is left alone** —
a typo should keep failing MkDocs' link check rather than become a
plausible-looking 404. So is a **directory under `docs/`**, which resolves to
that directory's page rather than to the tree ([§ The two link
gates](#the-two-link-gates) owns that case).

Both link syntaxes are rewritten — inline `](target)` and the reference-style
`[label]: target` definition. Python-Markdown resolves the two into the same
link, so covering only the first would leave the rarer form silently dead.

This was already needed before the backlog page: `design/` and `operations/`
shipped such links dead. Publishing the repo-internal docs made it load-bearing
— 724 links across the tree, 34 on `STATUS.md` alone.
`scripts/docs/source-links-hook-test.sh` asserts the rewrite in both directions under
`make check`.

### Unpublished is per build, not per path (Q561)

"Does not publish" is decided **per build, from its own file set** — `on_files`
records the src_uris whose `inclusion.is_included()`, the same derivation
[`hooks/backlog_link.py`](../../hooks/backlog_link.py) uses for the banner link.
Escaping `docs/` is only one way to qualify. The other is
[publication scope](#publication-scope): `plan/`, `development/` and `STATUS.md`
are pages on `dev` and absent from every release, so a `design/` page citing
`../plan/milestone-4.md` must resolve **in-site on `dev`** and **on github.com
from a release**. One markdown source, both renderings, no per-version editing:

| Scope | `../plan/milestone-4.md` becomes |
|---|---|
| `dev` | `../../plan/milestone-4/` — the published page |
| `stable`, every release | `{repo_url}/blob/{ref}/docs/plan/milestone-4.md` |

Fragments ride along verbatim (`#12-live-multi-tenant-validation-evidence…`),
which is why the two gates still get the last word on anchors: `make doc-links`
already validates them with GitHub's slugger, and GitHub's blob view is exactly
where a release now sends the reader.

**Why this can't be a gate instead.** MkDocs reports a link to an excluded page
as `Doc file 'X' contains a link to 'Y' which is excluded from the built site` —
and clamps it with `min(logging.INFO, validation.links.not_found)`, so
**no `validation` setting can raise it to a warning** and `--strict` will never
fail on one. It also still emits the relative URL, so the page ships a
live-looking link that 404s. Before this rewrite existed that was 120 links
across 22 operator pages on every numbered version. Handling it in the hook, per
build, is what makes the class of bug unreachable rather than merely fixed:
there is no link left to write incorrectly.

**`docs/README.md` is unpublished on every version, `dev` included** — MkDocs
drops it outright where an `index.md` shares the directory, so it never reaches
`files` at all (`exclude_docs` listing it only keeps that from surfacing as a
build warning). The hook absolutizes it to github.com, which is where that link
means to go anyway.

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
| Backlog label / status / size chips + per-row label clicks | the `Labels`, `St` and `Sz` columns of `docs/STATUS.md`'s Queue, Deferred, Flake watch and Progress tables |
| Scroll reveals | landing + `why-gag` pages only (skipped for `prefers-reduced-motion` / no-JS) |

**Keep those source markers intact** when editing — deleting the table column, a
blockquote, or a bold role lead silently breaks the matching site feature.

The backlog chips read the **rendered cells**, so they follow the backlog format
rather than duplicating it: labels are the backticked values in the `Labels`
column (each renders as its own `<code>`), status comes from the `St` emoji
(🔲 → Ready, 🚫 → Blocked; the Progress table's ✅/⚠️ → Done/Open), and size from
`Sz`. Adding a label to the
[legend](maintaining-backlog.md) needs no site change — a new value simply gets
its own chip. The tables are matched **by shape**: a `Labels` column *and* an
`ID`/`Item` first column. Both halves matter — the metric tables in
`design/02-architecture.md` and `operations/observability-metrics.md` also head a
column `Labels`, and the first-column test is what keeps chips off them.

A dimension every row shares is dropped rather than rendered, since selecting it
could only ever show the whole table: the Progress table's `Status` column is all
✅ today, so it gets no Status bar — and grows one automatically the moment a plan
goes ⚠️. The same rule means Deferred and Flake watch, which have no `St` column
at all, show only Label and Size.

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
