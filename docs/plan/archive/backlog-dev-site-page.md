# Filterable backlog page on the dev docs site (Q558)

Publish `docs/STATUS.md` on the docs site — **`dev` version only** — with label/status/size filter chips, so humans get a filterable backlog view while the markdown file stays the single source of truth that sessions, the linter, and the merge discipline operate on.
Filed 2026-07-31 from a backlog-visibility design discussion; this doc records the decisions so the implementing session doesn't re-litigate them.

## Status

**Complete** — shipped 2026-07-31.
All five acceptance criteria met; the durable how-to-maintain content is now [website.md § Publication scope](../../development/website.md#publication-scope) and [§ Links into the source tree](../../development/website.md#links-into-the-source-tree).

One thing this doc did not anticipate.
Publishing the repo-internal tree turned **724 relative links into source files** — `../cmd/agc/main.go` and friends, which resolve on github.com and 404 on the site — into published dead links, 34 of them on `STATUS.md` itself.
Criterion 1 ("working links") could not be met without a fix, so the change also adds [`hooks/source_links.py`](../../../hooks/source_links.py), which absolutizes any relative target escaping `docs/` against `repo_url`.
That was not scope growth for its own sake: `design/` and `operations/` had been shipping the same links dead on the public site all along, and the rewrite is one rule for the whole tree.
Remaining after it: 9 pre-existing INFO-level directory links, none broken.

## Decisions

### Markdown stays the single source of truth

Rejected: moving the backlog to HTML or a YAML source.
The lint gate, the isolated-commit merge semantics, the `<a id="QN">` anchors, PR-diff readability, and the backlog skill all operate on the markdown table.
The site is already "HTML that reads from a markdown source" — MkDocs renders it; a small script enhances the rendered table.
No second format, nothing to drift.

### Dev-only publication, via a conditional exclusion

Release versions are frozen builds — a backlog in them would be a snapshot from tag day, stale by construction.
The site's mike versioning already diverges content per version (each version builds from its ref; `dev` redeploys on every push to `main`), so dev-only is one conditional: MkDocs `!ENV` tags make the repo-internal `exclude_docs` entries conditional on an env var (`MKDOCS_EXCLUDE_DOCS`) that only the dev deploy path in [pages.yml](../../../.github/workflows/pages.yml) sets.
Release deploys never set it, including future tags, so no release version ever carries the page.

As implemented, the env var holds the exclusion *list* and the `!ENV` default is the full repo-internal set, so the two lists never duplicate.
`!ENV` resolves an env value as a scalar node verbatim (measured against `yaml_env_tag`), not by re-parsing it as YAML, so a multi-line default keeps its newlines.
The trap is that the default applies only when the variable is **absent**: a step-level `env:` with a falling-through `|| ''` would publish the internal docs on every release.
`pages.yml` `export`s it inside a conditional instead.

Bonus: `extra.version.default: stable` means Material already banners `dev` as "a different version" — visitors get the not-canonical framing for free.

**This bonus was wrong, and was measured wrong on the deployed site afterwards.** Material renders that banner only when a theme override fills its `outdated` block; the stock block is empty, and this repo defined no override, so no version carried the framing — `dev` included.
Nothing came for free.
The banner (and with it the only site-chrome route to this page) was added later; see [website.md § The version banner](../../development/website.md#the-version-banner).

### The whole repo-internal set publishes on dev, not STATUS.md alone

`STATUS.md` links densely into `docs/plan/` and `docs/development/`, which share the exclusion.
The build is not `--strict` (pages.yml runs plain `mkdocs build`), so STATUS-only publication would *succeed* with a page full of dead links.
The coherent policy: **`dev` = operator docs + contributor docs; releases = operator docs only.** The one env var conditions all the repo-internal `exclude_docs` entries together.

### Filter chips via the existing progressive-enhancement pattern

`docs/javascripts/extra.js` already implements persona filter chips over the `docs/operations/README.md` table ([website.md § Progressive enhancement](../../development/website.md#progressive-enhancement-docsjavascriptsextrajs)).
Apply the same pattern to the Queue and Deferred tables: chips for label, status, and size, parsing the rendered cells (backticked labels, the St/Sz columns).
Degrades to the plain table on github.com and without JS.
The website.md enhancement table gains a row naming the source markers it depends on.

As implemented the trigger is table *shape* — a `Labels` column plus an `ID`/`Item` first column — which picks up Flake watch and Progress for free.
The first-column half is load-bearing: the metric tables in `design/02-architecture.md` and `operations/observability-metrics.md` also head a column `Labels`, and would otherwise have sprouted chips.

### `feature` label

The vocabulary covers most work types (`bug` ≈ fix, `docs`, `tests`, `speed` ≈ perf, `infra` ≈ chore) but has no way to mark a feature — under a filterable view that's the first chip someone reaches for.
Add `feature` to the legend and retag the rows that warrant it (e.g.
Q554, the runner template library, which carried `docs` for want of anything better).
Stop there: importing the Conventional Commit taxonomy wholesale would create synonym pairs (`bug`/`fix`) with no query behind them.

### Reachability

[roadmap.md](../../roadmap.md) (published on all versions) links to the backlog with a version-pinned URL (`/dev/STATUS/`), labeled as the working backlog — honest from `stable`, where the page does not exist.

## Accepted wart

mike's version switcher keeps the current path, so switching from the dev backlog page to `stable` lands on a 404.
Inherent to any per-version page; acceptable for a contributor-facing view.

## Acceptance criteria

1. `/dev/STATUS/` renders on the published site with working links into `plan/` and `development/`; no release version (existing or future) gains any repo-internal page.
2. Queue and Deferred tables get label/status/size filter chips on the site and stay plain readable tables on github.com.
3. `feature` joins the STATUS.md label legend and is applied to current feature rows.
4. roadmap.md links to the dev backlog page.
5. website.md documents the new enhancement's source markers and the conditional-exclusion mechanism; the mkdocs.yml exclusion comment stays accurate.

## Non-goals

- Any change to the backlog file format, the linter, or the editing workflow.
- Publishing repo-internal docs on release versions.
- A standalone status/backlog web app or YAML data source.
