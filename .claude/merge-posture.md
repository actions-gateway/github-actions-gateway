---
reaches: opt-in-channel
gate: tag
reviewers: solo
merge: agent
human-review: breaking, feature, interface, security, performance, architecture, gate-weakening, dependencies, api/v*/**, api/api*/**, api/config/**, cmd/agc/api/**, cmd/gmc/api/**, cmd/agc/config/**, cmd/gmc/config/**, charts/**, mkdocs.yml, mkdocs-offline.yml, overrides/**, hooks/**, requirements-docs.txt, docs/*.md, docs/operations/**, docs/reference/**, docs/design/**, docs/assets/**, docs/stylesheets/**, docs/javascripts/**, docs/CNAME, docs/robots.txt, README.md, DESIGN.md, CONTRIBUTING.md, SECURITY.md, LICENSE, NOTICE, Dockerfile, docker-bake.hcl, coverage-baseline.txt, mk/gate-lists.mk, .gitattributes, scripts/agent/**, devtools/agent/**
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

- **The script's floor.** Whatever this list says, `merge-posture.py` never routes to the agent a PR touching `.claude/**` (this file included), any `CLAUDE.md` or `AGENTS.md`, `.github/workflows/**`, `.github/actions/**`, or a hook script `.claude/settings.json` registers.
  Those are the agents' own limits and what green means, so they are not repeated here.
- **Classes.** `breaking` and `feature` read off the title, and `dependencies` off the paths: every `go.mod`, `go.sum` and `vendor/` tree.
  `interface`, `security`, `performance`, `architecture` and `gate-weakening` are judged on the diff, so a security change in controller code, or a deleted test, reaches the maintainer even though no path glob names it.
- **API and CRDs.** `api/v*/**` and the two per-binary `cmd/*/api/**` trees are the CRD types, `api/api*/**` the exported label, name and condition constants, and `api/config/**` the generated CRDs.
  The rest of `api/` (`hack/`, the `Makefile`) is left out so tooling work does not read as an API change.
  `cmd/agc/config/**` and `cmd/gmc/config/**` hold the per-binary CRDs plus RBAC, webhook and admission-policy manifests.
  `charts/**` is what an operator installs: CRDs, RBAC, the admission guards, NetworkPolicy and the values surface.
- **The website.** The pages a released version publishes, from `mkdocs.yml`'s `exclude_docs`: the root pages (`docs/*.md`), `operations/`, `reference/`, `design/`, and the site's assets.
  Then the build itself: `mkdocs.yml`, `overrides/`, `hooks/`, and `requirements-docs.txt`.
- **Design docs.** `docs/design/**` and `DESIGN.md`.
- **Critical root docs.** `README.md`, `CONTRIBUTING.md`, `SECURITY.md` and the license files.
- **What ships.** The root `Dockerfile` and `docker-bake.hcl` decide what an image contains.
- **What a gate counts.** `coverage-baseline.txt`, `mk/gate-lists.mk` and `.gitattributes` decide coverage floors, which gates run, and how the merge drivers resolve conflicts.
- **The agents' own limits, beyond the floor.** `scripts/agent/**` and `devtools/agent/**`: the floor already catches the hook scripts `settings.json` names, but `pr-requeue-eligible` authorizes a re-enqueue without being a hook.

Deliberately not listed: `docs/queue/`, `docs/plan/`, `docs/development/` and `docs/postmortems/`.
The `dev` version publishes them, but they are contributor material and released versions exclude them, and nearly every PR closes a backlog row, so listing them would route everything to the maintainer.

In these globs `*` stays inside one directory and `**` crosses them, so a tree is written `dir/**` and `docs/*.md` is the root pages alone.
