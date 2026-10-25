---
reaches: opt-in-channel
gate: tag
reviewers: solo
merge: agent
human-review: breaking, feature, interface, security, performance, architecture, api/v*, api/api*, api/config/*, cmd/agc/api/*, cmd/gmc/api/*, cmd/agc/config/*, cmd/gmc/config/*, charts/*, mkdocs.yml, mkdocs-offline.yml, overrides/*, hooks/*, requirements-docs.txt, docs/index.md, docs/why-gag.md, docs/alternatives.md, docs/features.md, docs/demo.md, docs/getting-started.md, docs/roadmap.md, docs/operations/*, docs/reference/*, docs/design/*, docs/assets/*, docs/stylesheets/*, docs/javascripts/*, docs/CNAME, docs/robots.txt, README.md, DESIGN.md, CONTRIBUTING.md, SECURITY.md, CLAUDE.md, AGENTS.md, LICENSE*, NOTICE*, Dockerfile*, docker-bake.hcl, .github/workflows/*, .github/actions/*, *go.mod, vendor/*, tools/vendor/*, coverage-baseline.txt, mk/gate-lists.mk, .gitattributes, .claude/*, scripts/agent/*, devtools/agent/*
---

# Merge posture

Read by `session-orchestrator`'s smart review mode.
The schema is the skill's `references/merge-posture.md`; `scripts/merge-posture.py --route <repo> <pr>` applies it to one PR.

**`reaches: opt-in-channel`.** A merge to `main` deploys the website only to the `dev` version (`pages.yml`), which a reader reaches by choosing it in the version selector.
Images, the Helm chart and the `stable` site publish on a `v*` tag (`publish.yml`, `pages.yml`).

**`gate: tag`.** What users run is cut at the release, which re-validates the candidate on the dogfood cluster (`docs/operations/release.md`).
A wrong merge outside the `human-review` list costs a revert before the next tag.

**`reviewers: solo`.** One maintainer; the only other PR author is Dependabot.
`main` is guarded by rulesets, not classic branch protection, and they require status checks and the merge queue but no approving review.
There is no CODEOWNERS file.

**`merge: agent`.** The orchestrator may enqueue a PR that `human-review` does not catch.
Enqueueing is the merge here: the queue validates the candidate merge result and lands it.
How an agent enqueues is in `docs/development/parallel-dispatch.md` § The merge model.

## What still goes to the maintainer

The maintainer reviews API changes, anything that might affect security or performance, the website, critical documentation, and architecture.
Each part of the list maps to one of those:

- **Classes.** `breaking` and `feature` read off the title.
  `interface`, `security`, `performance` and `architecture` are judged on the diff, so a security change in controller code reaches the maintainer even though no path glob names it.
- **API and CRDs.** `api/v*` and the two per-binary `cmd/*/api/*` trees are the CRD types, `api/api*` the exported label, name and condition constants, and `api/config/*` the generated CRDs.
  The rest of `api/` (`go.mod`, `hack/`, the `Makefile`) is left out so a toolchain bump does not read as an API change.
  `cmd/agc/config/*` and `cmd/gmc/config/*` hold the per-binary CRDs plus RBAC, webhook and admission-policy manifests.
  `charts/*` is what an operator installs: CRDs, RBAC, the admission guards, NetworkPolicy and the values surface.
- **The website.** The pages a released version publishes, from `mkdocs.yml`'s `exclude_docs`: the seven root pages in the nav, `operations/`, `reference/`, `design/`, and the site's assets.
  Then the build itself: `mkdocs.yml`, `overrides/`, `hooks/`, and `pages.yml`.
  A new published page has to be added to the nav in `mkdocs.yml` or the strict build fails, so it is caught there even though its own path is not listed.
- **Design docs.** `docs/design/*` and `DESIGN.md`.
- **Critical root docs.** `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, the license files, and `CLAUDE.md` (with its `AGENTS.md` symlink), which governs every agent session in the repo.
- **What ships.** `Dockerfile`, `docker-bake.hcl` and the workflows decide what an image contains and how it is signed.
  A new module in any `go.mod`, `vendor/` or `tools/vendor/` is supply chain too, and the `architecture` judgement may not read it as one.
- **What green means.** The workflows produce the required checks the merge queue gates on, so weakening one weakens every later agent merge; ten of them can also route jobs to the self-hosted dogfood runner.
  `coverage-baseline.txt`, `mk/gate-lists.mk` and `.gitattributes` decide what a gate counts and how the merge drivers resolve conflicts.
- **The agents' own limits.** `.claude/*` (this file included, and the project's hook config), `scripts/agent/*` (the throttle and sub-agent hooks, and `pr-requeue-eligible`, which authorizes a re-enqueue) and `devtools/agent/*`.
  An agent may not merge a change to what it is allowed to do.

Deliberately not listed: `docs/queue/`, `docs/plan/`, `docs/development/` and `docs/postmortems/`.
The `dev` version publishes them, but they are contributor material and released versions exclude them, and nearly every PR closes a backlog row, so listing them would route everything to the maintainer.

Globs are `fnmatch`, whose `*` crosses `/`, so `docs/operations/*` covers the whole tree.
That is also why the root pages are named one by one: `docs/*.md` would match every backlog row.
`LICENSE*`, `NOTICE*` and `Dockerfile*` carry a `*` because the checker reads an entry with no `/`, `*` or `.` as a class name.
