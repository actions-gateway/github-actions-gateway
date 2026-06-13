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
| MkDocs scaffold + Pages deploy workflow | ❌ Open |
| Diagram conversion (ASCII → mermaid) | ❌ Open |
| Custom landing page | ❌ Open |
| "vs ARC" comparison page | ❌ Open |
| Annotated `ActionsGateway` CR example | ❌ Open |
| Social card + favicon from existing asset | ❌ Open |

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

**Decision:** ship as the project page (option 1). A vanity domain is **punted**
— the org move removed the personal-handle problem, so a custom domain is a
nice-to-have, not a need, and remains a trivial CNAME-only upgrade (option 2) if
we ever want one.

## Site map

```
/                     Landing page (hero + 4 pillars + vs-ARC + quickstart)
/why-gag/             The Problem -> The Solution, the comparison table, cost model
/getting-started/     <- docs/getting-started.md (the 10-minute path)
/docs/design/         <- docs/design/* (architecture, API, flows, security, glossary)
/docs/operations/     <- install, upgrade, runbook, troubleshooting, observability
/docs/development/    <- contributor guides (or keep these GitHub-only)
/capacity/            <- appendix-a SLOs + appendix-f cost model, made visual
```

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

## Build phases

1. Add `mkdocs.yml` + the Material dependency; point `docs_dir` at the existing
   `docs/` tree; wire a `pages.yml` GitHub Actions workflow that deploys to
   GitHub Pages on push to `main`. Set `site_url` to the project-page URL.
2. Convert the ASCII diagrams in README/architecture to mermaid.
3. Build the custom landing page (Material `home` override) from the approved
   mockup.
4. Add the "vs ARC" comparison page and the annotated CR example.
5. Add the OG card + favicon from the existing social asset.

## Open decisions

- **Developer docs on the site or GitHub-only.** `docs/development/*` is
  contributor-facing; it can render on the site or stay on github.com to keep
  the public site evaluator-focused. Lean: include them under a clearly-separate
  "Contributing" nav section.

## Out of scope (defer)

Interactive cost calculator, versioned docs via `mike` (revisit when 1.0 is
cut — see [release-1.0.md](release-1.0.md)), and an architecture explainer
animation are stretch ideas, parked in
[Appendix G](../design/appendix-g-future-enhancements.md) territory rather than
this first build.
