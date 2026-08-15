# Make the CI quality gates merge-blocking (required status checks)

**Status:** ✅ Complete — all nine workflow gate jobs are in place, and the ruleset has been updated to require them.
CI failures now block merges.

## Problem

The CI workflows assert correctness, security, and hygiene on every PR, and their own headers claim a failure blocks the merge — e.g. `security-scan.yml`: *"Both fail the PR so a regression cannot merge silently."* That guarantee did not hold (until this plan shipped).

- `main` has **no classic branch protection** (`GET .../branches/main/protection` → `404 Branch not protected`).
- Two active **rulesets** (`default`, `default-protect`) enforce only `deletion`, `non_fast_forward`, and `required_linear_history`.
  Neither has any `required_status_checks`, and there is no required-review rule.

So a red `security-scan` (or `unit-test`, `integration-test`, `e2e`, …) turns the PR red but does **not** stop a merge.
A reachable-CVE regression, a failing race test, or a broken manifest can merge silently — precisely the failure mode the workflow comments say is impossible.

## Why it isn't a one-line ruleset edit

Every gating workflow is **path-gated**: on a PR that doesn't touch its scope it either skips its jobs (internal `dorny/paths-filter` `changes` job) or does not trigger at all (top-level `on.<event>.paths` / `paths-ignore`).

GitHub keeps a required status check **Pending** when the workflow that would report it is skipped by path/branch filtering, and a Pending required check **blocks the merge forever**.
So you cannot simply mark `govulncheck` / `unit-test` / etc. as required — a docs-only PR would never report them and would wedge.

`e2e-calico.yml` already documents this exact tension and the fix: *"If it is ever made required, switch to the always-runs-then-skips `dorny/paths-filter` pattern `e2e-test.yml` uses so a non-matching PR still reports a green check."*

## Design: one always-running summary gate per workflow

Two coordinated pieces.

### 1. Each gating workflow always triggers, and exposes a single summary job

- **Remove the top-level path filter** (`paths-ignore` / `on.<event>.paths`) so the workflow triggers on *every* PR.
  Fine-grained skipping stays *inside* the workflow via the `changes` job + each real job's `if:` guard (unchanged), so the actual expensive jobs still don't run on unrelated PRs — only the cheap `changes` job does.
- Add a summary job whose id is **`<workflow>-gate`** (e.g.
  `unit-test-gate`) that `needs:` every real job and runs with `if: ${{ always() }}`.
  It passes only when every needed job concluded `success` or `skipped`, and fails on anything else (`failure`, `cancelled`):

  ```yaml
  unit-test-gate:
    # Aggregate required-status gate. Mark THIS job's check context required in
    # the branch ruleset — never the individual path-gated jobs (a path-skip
    # leaves them Pending, which blocks the merge forever). Runs on every PR
    # because the workflow no longer has a top-level path filter; passes when
    # each needed job succeeded or was legitimately skipped.
    needs: [changes, lint, shellcheck, vendor-check, tidy-check, unit-test, coverage]
    if: ${{ always() }}
    runs-on: ubuntu-latest
    timeout-minutes: 2
    steps:
      - name: require every gating job to have succeeded or skipped
        env:
          RESULTS: ${{ join(needs.*.result, ' ') }}
        run: |
          set -euo pipefail
          echo "needed job results: ${RESULTS}"
          for r in ${RESULTS}; do
            case "$r" in
              success | skipped) ;;
              *) echo "::error::a gating job concluded '${r}'"; exit 1 ;;
            esac
          done
  ```

  A matrix job (e.g.
  `trivy`) aggregates naturally: `needs: [trivy]` waits for all legs and `needs.trivy.result` is `failure` if any leg failed.

### 2. Mark the gate contexts required in the ruleset (admin)

After this PR merges, a repo admin adds each workflow's gate context to the `default-protect` ruleset's `required_status_checks`.
This is the only step that actually turns the gates merge-blocking, and it needs admin rights the CI branch does not have.

**Each gate job must have a globally unique id** — this is why they are `unit-test-gate`, `security-scan-gate`, … and not all just `gate`.
A normal job's check-run **name is its job id** (only reusable-workflow-call jobs get the `<caller> / <job>` form).
GitHub matches a required status check by that name and the ruleset UI dedupes candidates by name, so nine jobs all named `gate` collapse to a single indistinguishable `gate` entry that cannot be required per-workflow.
Distinct ids give nine distinct, individually-requireable contexts.

The required contexts to add (each is the gate job's id, verbatim):

```
unit-test-gate
security-scan-gate
integration-test-gate
manifest-validate-gate
license-notices-gate
status-lint-gate
plan-hygiene-gate
e2e-gate
e2e-calico-gate
```

Type each into the ruleset's "Require status checks to pass" search box (they appear once each workflow has reported the context at least once — i.e. after this PR runs).
Do **not** require the underlying jobs (`unit-test`, `e2e / e2e`, `trivy (…)`, …) directly — a path-skip leaves those Pending and wedges the merge; the gate is the skip-safe aggregate.

## Per-workflow edits

The chosen scope is the **full quality set** (everything that asserts correctness / security / hygiene).
Two shapes:

| Workflow | Gate job id | Current gating | Edit |
|---|---|---|---|
| `unit-test.yml` | `unit-test-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over lint, shellcheck, vendor-check, tidy-check, unit-test, coverage |
| `security-scan.yml` | `security-scan-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over govulncheck, trivy, polaris |
| `integration-test.yml` | `integration-test-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over integration-test |
| `manifest-validate.yml` | `manifest-validate-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over validate |
| `license-notices.yml` | `license-notices-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over check |
| `e2e-test.yml` | `e2e-gate` | internal `changes` + coarse `paths-ignore` | drop `paths-ignore`; add gate over e2e |
| `status-lint.yml` | `status-lint-gate` | top-level `on.paths`, no internal filter | add a `changes` job (same path list); gate `lint-status` on it; add gate |
| `plan-hygiene.yml` | `plan-hygiene-gate` | top-level `on.paths`, no internal filter | add a `changes` job (same path list); gate `plan-hygiene` on it; add gate |
| `e2e-calico.yml` | `e2e-calico-gate` | top-level `on.paths`, no internal filter; wraps `e2e-reusable.yml` via `uses:` | add a `changes` job (same path list); gate the reusable-workflow call on it; add gate |

`push: branches: [main]` triggers are unchanged; on push the real jobs still run unconditionally (post-merge gate), and `gate` passes when they do.

## Cost

Removing the coarse `paths-ignore` means a docs-only PR now spins the cheap `changes` job (and the trivial `gate` job) on the six always-changes workflows instead of skipping the workflow entirely — a few seconds of `ubuntu-latest` per workflow, no Go build, no image build, no cluster.
This is the standard, GitHub-documented cost of making a path-gated workflow safe to require.

## Verification

- `actionlint` clean on every edited workflow.
- On a docs-only PR: every `changes`/`gate` job runs, all real jobs skip, every `gate` reports green → PR is mergeable (no wedge).
- On a code PR that fails a real job: that workflow's `gate` goes red.
- Once the gates are required via the ruleset: a PR with a red `gate` cannot be merged; a docs-only PR with all-green gates can.
