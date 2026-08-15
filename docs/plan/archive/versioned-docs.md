# Versioned docs (mike)

**Status:** ✅ Done — shipped for Q238 (supersedes the Q387 tag-trigger fix).

Decision log for making [actions-gateway.com](https://actions-gateway.com) a versioned docs site.
The durable how-to-maintain reference is [website.md § Versioned deploy](../../development/website.md#versioned-deploy-mike); this file records *why* the design is shaped the way it is.

## Problem

`pages.yml` deployed the site on **every push to `main`**, so a feature whose code merged but whose Helm chart had not yet been tagged/released appeared as "Available now" on the live site (Q387).
Operators installing the released chart couldn't find it.
The deeper issue (Q238): a single `main`-rendered page can't be correct for all readers at once — a released operator and a `main` reader need different install/config steps.

## Approach

Adopt [`mike`](https://github.com/jimporter/mike), MkDocs Material's first-party versioning tool, so the site carries one built copy per version with a native version selector:

- **`stable`** — an alias of the latest stable release tag; the **default** the root `/` redirects to.
  Operators land here.
- **older releases** (`1.0.0`, …) — reachable via the selector.
- **`dev`** — the unreleased `main` branch; opt-in via the selector, kept out of search results (`robots.txt` `Disallow: /dev/`).

`stable` being the default is what fixes Q387: unreleased `main` content is only ever visible under the opt-in `dev` version, never as the released docs.

## Key decisions

- **mike over a hand-rolled two-track build.** Material renders the version selector and the "you're viewing a different version" banner natively from mike's `versions.json` (`extra.version.provider: mike`).
  Reusing the standard tool beats reinventing the selector.
- **Pages source stays "GitHub Actions" (not branch-serving).** mike maintains the version tree on the `gh-pages` branch; the `publish` job serves that tree as the Pages **artifact**.
  This keeps full control of the artifact root, which matters for the custom domain: mike does **not** keep a root `CNAME`, so the workflow re-asserts `docs/CNAME` at the artifact root every deploy.
  Branch-serving would have required a server-side Pages-source change and risked mike clobbering the root CNAME.
- **`--alias-type=copy` for aliases.** GitHub Pages artifact deploys don't follow symlinks, so mike's default symlinked `stable/` would 404 on deep links.
  `copy` makes `stable/` a real directory; `stable/operations/…` resolves.
- **Trigger model mirrors `publish.yml`.** A stable `v*` tag deploys the release; a prerelease tag (`0.x`, `-rc`/`-alpha`/`-beta`) does not deploy — the same prerelease test `publish.yml` uses (Q293), so the site republishes on exactly the tags that ship a stable chart.
  A `main` push refreshes `dev`.
  `workflow_dispatch` deploys an explicit `version`/`alias` (for seeding and manual redeploys).
- **`stable`/default is semver-aware, not latest-tag-wins.** A stable tag claims the `stable` alias + the default root redirect **only when it is the highest released version** (`mike list` → `sort -V`).
  This handles patch releases: a backport to an older supported line (e.g.
  `v1.2.5` cut after `v1.3.0`) publishes its own version without demoting the site from `1.3.0`.
  Because `mike` builds each version from its tag, a patch tagged off the release line (not off feature-ahead `main`) carries only that line's content — so correct release engineering makes the docs correct for free; no docs-specific branch is needed.
  See [release.md § Patch releases and backports](../../operations/release.md#patch-releases-and-backports).

## Rollout

Releases cut before this workflow landed (`v1.0.0`, `v1.1.0`) aren't in the mike tree yet, and the root has nothing to redirect to until a `stable` version exists.
Seed them once via `workflow_dispatch` — see [website.md § Seeding already-released versions](../../development/website.md#seeding-already-released-versions).

## Validation

The deploy path only runs on push/tag, so the PR's CI does not exercise it.
It was validated locally against an isolated clone (no `--push`, real origin untouched): mike deploy of `dev` + `1.0.0` + `1.1.0 stable`, `set-default stable`, then `git archive gh-pages` + CNAME injection — confirming the root redirect to `stable/`, a correct `versions.json`, a real (non-symlink) `stable/` directory with working deep links, and the injected root `CNAME`.
The `resolve` step's event/ref→version mapping was table-tested across `main`, stable tag, prerelease tag, and each `workflow_dispatch` input shape.
Cross-run **accumulation** was verified too (a fresh clone with no local `gh-pages` deploys a new version onto the existing ones via `origin/gh-pages`, which `fetch-depth: 0` makes available).
The semver-aware `stable` decision was table-tested across first release, forward release, backport-after-newer-minor (keeps `stable`), double-digit ordering (`1.10.0` > `1.9.0`), and re-run idempotency.

## Follow-ups

- **First-deploy seeding is manual** (the two `workflow_dispatch` runs above) — acceptable one-time cost, documented in website.md.
- **Old flat URLs → versioned paths.** Inbound links to the pre-versioning flat paths (`/operations/…`) now live under `/stable/operations/…`; mike's root redirect covers `/` but not deep flat links.
  The site is new (launched Q129), so link rot is limited; per-page redirects were judged not worth the complexity.
