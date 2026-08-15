# Regrouping `scripts/` so a path prefix carries the blast radius

**Status:** ✅ Complete (2026-08-01, Q571).
All 99 top-level files moved into ten groups, 762 references rewritten across 190 files, every filter that named a script converted to a prefix glob, and the interim `nondocs` filter deleted.
What the implementation changed about the plan is in [What shipped](#what-shipped).

## The problem

`scripts/` is a flat namespace of 100 top-level files plus three subdirectories (`dogfood/`, `lib/`, `updatecli/`).
Nothing about a script's path says which CI gate cares about it, so every path filter that wants "the scripts this tier runs" has to either enumerate them or take the whole directory.

Both choices are bad, and the repo has one of each:

| Workflow | Scripts patterns | Consequence |
|---|---|---|
| `e2e-test.yml` | `scripts/*` + `scripts/!(dogfood)/**` | Any top-level script triggers a ~13 min e2e run |
| `e2e-calico.yml` | `kind-with-registry.sh`, `bake-with-retry.sh` | Only 2 of the ≥6 scripts its lane actually uses |
| `security-scan.yml` | `go-vulncheck.sh`, `polaris-scan.sh`, `lib/**` | Enumerated, drifts silently |

The two e2e lanes **run the same `e2e-reusable.yml`** yet disagree about which scripts matter by roughly 60×.
They cannot both be right.
`e2e-reusable.yml` directly references six scripts — `bake-with-retry.sh`, `download-verified.sh`, `free-runner-disk.sh`, `kind-with-registry.sh`, `prepull-manifest-images.sh`, `pull-image-with-retry.sh` — and reaches more through the `make` targets it invokes, so the calico list looks like a latent Q400-class false negative: a change to `free-runner-disk.sh` skips the lane that would exercise it.

`scripts/dogfood/` is the tell that this is structural rather than a filter bug.
It exists precisely so the e2e filter can exclude it, and excluding it needs an extglob (`scripts/!(dogfood)/**`) with a nine-line comment explaining why a leading `!` does not subtract in `dorny/paths-filter`.

### What it cost, concretely

Measured during Q561 (2026-08-01):

- PRs #1066 and #1071 changed a MkDocs hook, its unit test, and a doc page.
  `docs/**` and `hooks/**` are absent from the e2e filter, so the sole match was `scripts/*` catching what was then `scripts/source-links-hook-test.sh` (now [`scripts/docs/source-links-hook-test.sh`](../../../scripts/docs/source-links-hook-test.sh), where it cannot match).
- #1063 is the control: docs + `hooks/backlog_link.py` + `mkdocs.yml`, no script — e2e correctly skipped.
- On #1071 the unnecessary run **failed** `E2E_Migration_MigratedTenantReconciles‑ IntoAWorkingControlPlane` (timeout; 61 of 62 specs passed) while `main` was green across the 8 preceding runs.
  A documentation change was blocked by a flake it had no business meeting ([Q570](../../STATUS.md#Q570)).

### Prior art

[structural-debt-audit-2026-07.md](../structural-debt-audit-2026-07.md)'s **F10** (shipped as Q370) covered *script-layer* sprawl — oversized scripts and `scripts/lib/common.sh` adoption.
That is a different axis: it made individual scripts smaller and more uniform, and left the flat namespace alone.
Nothing in it addresses which gate a script belongs to.

Its own numbers show the namespace growing rather than settling: the audit counted **69** scripts on 2026-07-20; `shellcheck-scripts.sh` reports **122** today.
Filters that already had to enumerate or over-match are covering a set that nearly doubled in under two weeks.

## The approach

Group by **blast radius** — which gate consumes the script — so every filter becomes a plain prefix glob and the extglob disappears:

| Directory | Holds | Gated by |
|---|---|---|
| `scripts/e2e/` | cluster bring-up, image bake/pull, chart install checks | e2e, e2e-calico |
| `scripts/docs/` | doc/backlog/site tooling and the MkDocs hook tests | doc-links, status-lint, plan-hygiene |
| `scripts/go/` | build, test, lint, vendor, coverage | unit-test |
| `scripts/security/` | vulncheck, trivy, polaris | security-scan |
| `scripts/release/` | publish, sign, third-party notices | publish |
| `scripts/agent/` | Claude hooks, local throttle | none (never CI-gating) |
| `scripts/ci/` | the meta-gates: path filters, tool check, conflict markers | unit-test |
| `scripts/dogfood/` `scripts/lib/` `scripts/updatecli/` | unchanged | — |

Two properties make this worth the churn:

1. **A filter becomes `scripts/e2e/**`.** No enumeration to drift, no extglob.
2. **A `*-test.sh` inherits its subject's gate** by sitting beside it.
   That is the specific defect above: `source-links-hook-test.sh` belongs to the docs site, and in `scripts/docs/` it could never have triggered e2e.

Nothing stays at the top level except `README.md`, so "which gate cares?" has to be answered when a script is added rather than defaulting to the catch-all.

## Scope and cost

The move itself is mechanical; the references are the work.

| Surface | Count |
|---|---|
| References from outside `scripts/` | **550**, across **102 files** |
| References within `scripts/` | 237 |

The 102 files are not only workflows and the `Makefile`: they include `CLAUDE.md`, `CONTRIBUTING.md`, `.githooks/pre-commit`, `.claude/settings.json`, `.golangci.yml`, `.gitignore`, chart READMEs, generated CRD YAML, and Go godoc comments.
Every one has to move with its file.

Runners need almost no change — `shellcheck-scripts.sh` and `make scripts-test` already recurse (they pick up `scripts/dogfood/*` and `scripts/updatecli/*` today).

## Risks

- **Conflict blast radius.** A ~100-file rename conflicts with any concurrent branch touching `scripts/`.
  Land it in a quiet window, in one PR, rebased immediately before merge.
- **A missed reference is a broken gate, not a compile error.** Shell resolves paths at run time.
  `make check` plus the CI matrix covers most of it; the residual risk is a path referenced only by a rarely-triggered workflow.
- **Filter narrowing is the Q400 hazard.** Rewriting `scripts/*` to `scripts/e2e/**` is a *narrowing* — the exact move `check-path-filters.sh` exists to police.
  Classify every script deliberately; when unsure, put it where the broader gate sees it.
- **`git log --follow` / blame continuity** degrades for 100 files.

## Success criteria

1. No top-level `scripts/*.sh` remains; `scripts/README.md` documents the groups.
2. `e2e-test.yml`'s scripts patterns are prefix globs; the `scripts/!(dogfood)/**` extglob and its comment are gone.
3. `e2e-test.yml` and `e2e-calico.yml` agree on the script set behind `e2e-reusable.yml`, with a `check-path-filters.sh` assertion holding them to it.
4. The interim `nondocs` filter in `e2e-test.yml` is deleted.
5. `make check`, `make path-filters-check`, and a full CI matrix are green, and a docs-only PR is observed skipping e2e.

## What shipped

All five criteria met.
Three things the plan got wrong, and one it did not anticipate.

### The group table needed three more directories

The plan's seven groups left four gates with nowhere to put their scripts.
The shipped set is ten:

| Added | Holds | Why the plan's table could not hold it |
|---|---|---|
| `fetch/` | `retry.sh`, `download-verified.sh`, `pull-image-with-retry.sh`, the two pre-pull cachers | The plan filed image bake/pull under `e2e/`, but these five are called by e2e, security-scan, publish, manifest-validate, autoscaler-drift and the `$(COSIGN)` rule. Under `e2e/` they would be invisible to every other gate — the Q400 narrowing in a new shape |
| `manifest/` | `manifest-validate.sh` + the three `sync-chart-*.sh` generators | `manifest-validate.yml` is a gate the plan's table omits, and its four backends map to it exactly |
| `dev/` | `setup.sh`, the two Milestone-1 probes, the two GKE spikes | The plan's `agent/` is "Claude hooks, local throttle"; these are developer-only but not agent tooling |

`ci/` also came out narrower than the plan implied: it holds the meta-gates and `run-parallel.sh`, not the shared CI plumbing, which is the whole point of `fetch/` existing separately.
`ci/**` is in no heavy tier's filter; `fetch/**` is in all of them.

### `autoscaler-drift.yml` was not in the table either

`autoscaler-cluster.sh` and `karpenter-cluster.sh` are live-cluster bring-up, so they went to `e2e/`.
That makes `scripts/e2e/**` over-trigger the kindnet e2e lane on a change to either — accepted deliberately: both files are touched only by the weekly updatecli pin bump, and over-triggering is the safe direction.

### Filters got *wider*, not narrower, wherever a group maps to a gate

The plan's stated risk was narrowing.
In practice the enumerated filters were the ones at risk, and collapsing them to a prefix glob widened them: `unit-test.yml:code` went from five named scripts to `scripts/go/**`, `security-scan.yml` from one to `scripts/security/**` + `scripts/fetch/**`, `manifest-validate.yml` from four to `scripts/manifest/**` + `scripts/fetch/**`.
Only `e2e-test.yml` narrowed, and only there does criterion 3's new assertion apply.
`autoscaler-drift.yml` gained `scripts/fetch/**` outright — it runs `download-verified.sh` and had never listed it.

### The move breaks two things a rename normally does not

- **Nine scripts computed `REPO_ROOT` as `dirname "$BASH_SOURCE"/..`**, which is now one level short.
  Shell resolves that at run time, so nothing failed until the script ran — exactly the residual risk the plan named.
  Grep for the form before moving a script, not after.
- **`git-merge-status.sh` resolved its awk rules through `$SCRIPT_DIR/lib/`**, and `make merge-driver` writes the driver's path into per-clone git config.
  A clone that installed the driver before this change has a dead path in its config and must re-run `make merge-driver`; `scripts/README.md` says so.

### The new assertion, verified by breaking it

`check-path-filters.sh` assertion 4 pairs `e2e-test.yml:e2e` with `e2e-calico.yml:calico` and compares their `scripts/` patterns as a set.
Deleting `scripts/fetch/**` from the Calico lane and re-running the gate reproduces the exact historical defect and fails with a diff of the two sets; restoring it returns green.
Four fixture cases cover subset, superset, order-insensitivity and the non-`scripts/` patterns the two lanes are meant to differ on.
