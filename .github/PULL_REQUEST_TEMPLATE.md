<!--
Thanks for contributing! A few conventions for this repo:
- Commit messages follow Conventional Commits (feat(agc):, fix(gmc):, docs:, ...).
- Reference Queue items by their bare ID (Q44, never #44 — the bare form keeps
  GitHub from auto-linking to PR/issue 44).
- Keep the PR scoped to one concern; unrelated work goes in its own PR.
-->

## What & why

<!-- What does this change do, and why? Link the motivating issue or Queue item (e.g. Q44). -->

## How it was tested

<!-- Commands run and their result: `make check`, plus any heavier tier the change
     warranted (integration/e2e, `make test-race`, `make vulncheck`, `make trivy-scan`). -->

## Checklist

- [ ] `make check` is green (plus any heavier gate this change warranted).
- [ ] The diff matches the design intent and has no stray debug code, TODOs, or unrelated changes.
- [ ] Docs are updated per the [doc-update matrix](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/development/doc-update-matrix.md) — design **and** operator-facing docs when behavior an operator configures or observes changed.
- [ ] This change introduces no security regression as a default (or the trade-off is called out and signed off).
- [ ] For path-gated CI (integration/e2e/security): I confirmed the relevant heavy gates actually **ran** (green-because-skipped is not enough).
