# Project website (GitHub Pages)

A public website for GitHub Actions Gateway (GAG): a marketing landing page that
converts an evaluator in under a minute, plus a searchable rendering of the
existing `docs/` tree. The goal is to do what GitHub's file browser cannot —
real full-text search, rendered diagrams, a punchy "vs ARC" comparison, and a
front door that answers *"is this worth replacing Actions Runner Controller
(ARC) for?"* before the visitor leaves.

This is net-new scope tracked as STATUS Queue [Q129](../STATUS.md). It is **not**
a 1.0 gate. It depends on no code changes; it is additive tooling plus content
assembled from docs that already exist.

## Status at a glance

| Work item | Status |
|---|---|
| Tooling decision (MkDocs Material) | ✅ Decided (this doc) |
| Domain decision | ✅ Decided — project page now, vanity domain punted (see below) |
| MkDocs scaffold + Pages deploy workflow | ✅ Shipped — `mkdocs.yml`, `overrides/`, `requirements-docs.txt`, `.github/workflows/pages.yml` |
| Architecture diagram (mermaid) | ✅ Shipped on the landing page (existing ASCII diagrams render as-is) |
| Custom landing page | ✅ Shipped — `docs/index.md` (hero, pillars, listener-overhead stat) |
| "vs ARC" comparison page | ✅ Shipped — `docs/why-gag.md` |
| Annotated `ActionsGateway` CR example | ✅ Shipped — in `why-gag.md`, fields verified against `getting-started.md` |
| Social card from existing asset | ✅ Shipped — OG/Twitter card via `overrides/main.html`; favicon left at Material default |
| Public launch | ✅ Launched 2026-06-16 — Pages enabled, deployed via `workflow_dispatch`; landing + banner reflect the `v1.0.0` GA install and keep the Q99 capacity caveat |
| Cross-tree link reconcile (build `--strict`) | ❌ Follow-up, folded into [Q52](../STATUS.md) — see below |

## Audience and the one job

The visitor is a **platform / SRE lead already running or evaluating ARC** who
has hit one of GAG's four pain points: quota starvation, listener overhead, no
eviction recovery, or egress isolation. They arrive with a single question —
*"is this worth replacing ARC for?"* — and the front page must answer it fast,
then funnel to either **Get started** (try it) or **Why GAG / vs ARC** (justify
it to their team).

The trap to avoid: a wall of prose that restates the README. The README is
already strong. The site exists to do what GitHub markdown can't.

## Tooling decision: MkDocs Material

The `docs/` tree is already well-structured markdown with ASCII diagrams, a
glossary, and design/operations/development sections — exactly what
[MkDocs](https://www.mkdocs.org/) + the
[Material theme](https://squidfunk.github.io/mkdocs-material/) renders well. It
gives us, nearly for free:

- Instant client-side search across all docs (the single biggest upgrade over
  browsing files on github.com).
- Mermaid diagram rendering (convert the ASCII art blocks to real diagrams).
- Code copy buttons and tabbed code blocks (Helm vs `kubectl` install paths).
- Auto nav from the existing folder layout — minimal restructuring.
- Built-in dark mode, and versioning via [mike](https://github.com/jimporter/mike)
  once 1.0 is cut.
- Deploys to GitHub Pages via a short Actions workflow.

Alternatives considered and rejected:

| Option | Verdict |
|---|---|
| **MkDocs Material** | ✅ Best fit — renders existing markdown, strong search, infra-community standard. |
| Plain Jekyll (Pages-native) | Zero build, but weak search and dated theming; fights us on the landing page. |
| Docusaurus | Capable, but React/MDX-heavy — overkill without a blog or heavy interactivity. |
| Hand-rolled static HTML | Max control, max maintenance — only if the landing page mattered far more than docs. |

Chosen shape: a **custom-styled landing page** (the marketing surface, via a
Material `home` template override) that links into the **MkDocs-rendered docs**
for everything else. Best of both — designed front door, low-maintenance docs.

## Domain decision (affected by the org move)

The project now lives at `github.com/actions-gateway/github-actions-gateway`
(moved from a personal account). That changes the calculus:

- **Before** (personal account): the only no-custom-domain URL was a
  personal-handle project page, e.g. `karlkfi.github.io/github-actions-gateway`
  — long and tied to an individual.
- **After** (org): the org owns a branded namespace. The project page is now
  `https://actions-gateway.github.io/github-actions-gateway/` — project-scoped
  and brandable, no personal handle.

Options, in order of preference:

1. **Project page on this repo** (recommended): serve at
   `actions-gateway.github.io/github-actions-gateway/`. Zero new repos, site
   lives beside the code so docs never drift. Set `site_url` accordingly and
   use a relative base path.
2. **Custom domain** (e.g. `actions-gateway.dev`): a single `CNAME` file on this
   repo plus DNS records. The org move makes this a clean *later* upgrade rather
   than a need — no rebuild, no content change, just DNS. Recommend deferring
   until/unless we want a vanity domain.
3. **Bare org apex** `actions-gateway.github.io/`: requires a *separate* repo
   named `actions-gateway.github.io`. Cleanest URL, but it decouples the site
   from the code repo and reinvites docs drift. Not worth it for a single
   project — reconsider only if the org grows to host multiple projects.

**Decision:** shipped initially as the project page (option 1), then upgraded to
the **custom domain** (option 2) in 2026-06 once `actions-gateway.com` was
purchased. The site now serves from the apex `https://actions-gateway.com/`: a
`docs/CNAME` file (copied verbatim into the build output) plus
`site_url: https://actions-gateway.com/` and the registrar DNS records (apex `A`
→ `185.199.108–111.153`, `AAAA`, `www` `CNAME` → `actions-gateway.github.io`).
As predicted, the move was DNS + two file changes — no rebuild or content
rewrite. DNSSEC and org-level domain verification remain optional future
hardening. See [`docs/development/website.md`](../development/website.md#custom-domain)
for the durable how-to.

## Site map

```
/                     Landing page (hero + 4 pillars + vs-ARC + quickstart)
/why-gag/             The Problem -> The Solution, the comparison table, cost model
/getting-started/     <- docs/getting-started.md (the 10-minute path)
/docs/design/         <- docs/design/* (architecture, API, flows, security, glossary)
/docs/operations/     <- install, upgrade, runbook, troubleshooting, observability
/capacity/            <- appendix-a SLOs + appendix-f cost model, made visual
```

**Publication scope.** Publish `docs/design/` and `docs/operations/` (plus the
landing + comparison pages). Exclude `docs/plan/` and `docs/STATUS.md` (internal
planning) and `docs/development/` (contributor docs) — see the decision below.

## Page-by-page content

- **Landing page.** Hero line + subhead, the one-command `helm install`, the
  four pillars as cards, and *one* hero stat that makes the value visceral — the
  listener-overhead delta (~2.5 GiB / 10 pods for ARC vs ~600 KiB / 1 pod for
  GAG, at 10 runner groups), or a cost curve from
  [appendix-f-cost-model.md](../design/appendix-f-cost-model.md). Footer CTAs.
- **Why GAG / vs ARC** — the highest-leverage conversion page. A clean
  comparison matrix (priority tiers / eviction retry / per-tenant egress /
  listener cost / scale-to-zero), each row linking to the design doc that backs
  the claim. This is what gets pasted into a team's decision doc. Relates to
  [Q60](../STATUS.md) (competitive analysis) — keep the marketing comparison
  here and the rigorous analysis in
  [appendix-d](../design/appendix-d-alternatives-considered.md).
- **Architecture** — convert the README's ASCII four-tier diagram to a rendered
  mermaid/SVG diagram (GMC → AGC → proxy → worker), each tier clickable into its
  design section.
- **Annotated `ActionsGateway` CR** — syntax-highlighted, commented YAML showing
  how little a tenant writes to get a fully isolated gateway. The "one CR does
  all this" demo moment.

## Brand / visual identity

Reuse [`docs/assets/social-preview.svg`](../assets/social-preview.svg) as the
design language — palette and mark — so the social/OG card, favicon, and header
logo stay consistent. Infra-tool convention: clean, monospace accents for
commands, a single accent color, generous whitespace, dark mode. Set the
OG/Twitter card to the existing social preview so shared links look intentional.

## Implementation notes (shipped)

The scaffold builds locally and in CI with `mkdocs build` on pinned
`mkdocs==1.6.1` + `mkdocs-material==9.7.6` (Material 9.x caps `mkdocs < 2`; the
new MkDocs 2.0 is incompatible — pin exact until that settles). Specifics:

- **Heading slugs.** `toc.slugify` is set to the GitHub-compatible
  `pymdownx.slugs.slugify(case=lower)` so the in-repo docs' GitHub-style anchor
  links resolve on the site too. This cleared the bulk of the anchor mismatches.
- **Known broken links on the site (build warnings, not errors).** Two classes
  remain and are deliberately deferred: (1) ~19 stale intra-doc anchors that are
  also wrong on github.com (pre-existing doc bugs); (2) links from docs into
  repo files outside `docs/` (`../charts/…`, `../../.github/…`, `cmd/…`) that
  resolve on github.com but 404 on the standalone site. The build is **not**
  `--strict`, so these warn without failing. Reconciling them — and then flipping
  the CI build to `--strict` as a link gate — folds into [Q52](../STATUS.md)
  (markdown link + anchor CI gate). Do this as part of the pre-launch pass.
- **Launch procedure (gated on Q99).** The `pages.yml` deploy job only runs on
  `workflow_dispatch`; `pull_request`/`push` merely validate the build. To
  launch: land Q99's claim fixes, reconcile the landing/comparison copy, enable
  Pages (Settings → Pages → Source: GitHub Actions), then run the workflow.

## Maintaining the site

Durable maintenance conventions — the brand-asset generator, the
progressive-enhancement JS, and the persona/audience two-places sync rule — live
in [`docs/development/website.md`](../development/website.md), deliberately kept
out of this plan doc so they survive its eventual archival.

## Build phases

1. Add `mkdocs.yml` + the Material dependency; point `docs_dir` at the existing
   `docs/` tree; wire a `pages.yml` GitHub Actions workflow that deploys to
   GitHub Pages on push to `main`. Set `site_url` to the project-page URL.
2. Convert the ASCII diagrams in README/architecture to mermaid.
3. Build the custom landing page (Material `home` override) from the approved
   mockup.
4. Add the "vs ARC" comparison page and the annotated CR example.
5. Add the OG card + favicon from the existing social asset.

## Decided

- **No contributor docs / "Contributing" section on the site (for now).**
  `docs/development/*` stays GitHub-only. Publishing contributor docs implies
  inviting contributions, which needs a secure intake flow first — AI review and
  implementation, with possible human gating — and that is a separate, non-trivial
  design we have not done. Revisit when (and if) that contribution flow is
  designed; until then the public site stays evaluator-focused.

## Decided (sequencing)

- **Build in parallel with [Q99](../STATUS.md); gate only the public launch.**
  The site work (scaffold, tooling, nav, diagram conversion, page structure) has
  no dependency on Q99 and proceeds now. The rendered docs pages render `docs/`
  source directly, so they inherit Q99's claim fixes automatically with no
  site-side change. The only surface that *duplicates* the claims Q99 flags
  ("thousands of sessions", egress-blocked) is the hand-authored landing + "vs
  ARC" copy — reconcile that against the corrected source as a final pass before
  flipping Pages public / announcing. So Q99 gates the launch, not the work.

## Open decisions

- ~~**Pre-1.0 maturity banner.**~~ **Resolved:** the alpha banner ran until 1.0;
  on the GA launch (2026-06-16) it was replaced with a "v1.0.0 is here" notice
  that still links the Q99 capacity caveat (`overrides/main.html`).
- **Analytics.** *Lean:* none for v1 (avoids a privacy/consent surface); add
  later if traffic matters.

## Out of scope (defer)

Interactive cost calculator, versioned docs via `mike` (revisit when 1.0 is
cut — see [release-1.0.md](release-1.0.md)), and an architecture explainer
animation are stretch ideas, parked in
[Appendix G](../design/appendix-g-future-enhancements.md) territory rather than
this first build.
