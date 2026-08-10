# Q507 — No gate upgrades from the last released chart tag

**Status: SHIPPED.** [`scripts/e2e/chart-released-upgrade-check.sh`](../../../scripts/e2e/chart-released-upgrade-check.sh) (`make chart-released-upgrade-check`) runs last among the chart checks in `e2e-reusable.yml` (kindnet leg only).
The design question below resolved to option (2) — `helm pull` of the published OCI artifact — with the tag discovered dynamically as the highest stable `vX.Y.Z` tag on origin (`git ls-remote`, prereleases excluded; the version `publish.yml` keys the chart on).
No stable tag → clean skip; a stable tag with no published chart → loud failure, since operators cannot install that release either.
The values migration path (acceptance bullet 4) is covered: an upgrade carrying the removed `priorityClassAllowlist.configMapName` must fail at render with the migration message, anchored on the guard still existing in HEAD's templates so retiring the guard retires the probe.
Gate contract for chart authors and the discovery rules: [testing.md § The released-chart upgrade gate](../../development/testing.md#the-released-chart-upgrade-gate-q507).

## The gap

Three paths exercise the chart, and none of them upgrades a **released** release:

| Path | What it installs | What it upgrades from |
|---|---|---|
| `make deploy` / e2e | HEAD's chart | nothing — always a first install |
| [`chart-upgrade-check.sh`](../../../scripts/e2e/chart-upgrade-check.sh) (Q475) | HEAD's chart | HEAD's chart with two markers injected |
| [`chart-reinstall-check.sh`](../../../scripts/e2e/chart-reinstall-check.sh) (Q444/Q492) | HEAD's chart | uninstall → reinstall, same chart |

So every gate answers "does HEAD upgrade to HEAD?" — never "does the chart an operator is actually running upgrade to HEAD?".
Those are different questions whenever a change interacts with what Helm does *between* two chart versions.

## What it let through

Q492 moved the PriorityClass guard's `paramKind` to a CRD and shipped that CRD in the chart-root `crds/` dir — the only placement Helm installs early enough for a CR in the same release.
Helm installs `crds/` on `helm install` **only, never on upgrade**, so **every** existing release failed to upgrade:

```text
Error: UPGRADE FAILED: resource mapping not found for name: "gmc-priorityclass-allowlist"
no matches for kind "PriorityClassAllowlist" in version "actions-gateway.com/v2beta1"
ensure CRDs are installed first
```

`make check` was green, every path-gated heavy gate ran and passed, and the PR was marked review-ready with a description asserting that installs which never set `priorityClassAllowlist.configMapName` needed no action.
It was caught by a question in review, not by a gate.
Nothing in CI could have caught it, because nothing in CI upgrades from a released chart.

The fix ([#958](https://github.com/actions-gateway/github-actions-gateway/pull/958)) was verified by hand: `git archive v1.2.0` into a scratch dir, install on kind, upgrade to HEAD.
That manual procedure is what this item automates.

## The design question

Everything else is mechanical; this is the part to settle first.

**How does CI resolve "the last released chart"?** Three candidates, with the trade-off that matters:

1. **`git archive <latest tag>`** — no network, works on a fork and offline, and the chart is exactly what the tag contains.
   But it is the chart *source*, not the published artifact, so it cannot catch a packaging difference.
2. **`helm pull oci://ghcr.io/...` at the latest published version** — tests the real artifact, which is what operators run.
   Needs registry access from CI and a rule for what to do when the newest tag has not published yet.
3. **A pinned floor in-repo** (e.g. a `MIN_UPGRADE_FROM` file) — deterministic and reviewable, but rots silently unless something reminds you to bump it.

(1) is the cheapest starting point and would have caught Q492's defect outright.
(2) is the honest end state.
They are not exclusive — (1) as the gate, (2) as a periodic job — and the choice can be deferred to implementation.

Second-order: the gate must skip cleanly when no tag exists yet (a fresh fork), and must not pin the repo to a tag so old it predates a deliberate breaking change.
Both argue for resolving the tag dynamically rather than hardcoding.

## Acceptance

- A gate installs the last released chart on a kind cluster, upgrades it to HEAD, and fails if the upgrade fails or admission is broken afterwards.
- It fails on a reintroduction of the Q492 shape — a chart-root `crds/` CRD whose CR the same release renders, with no documented apply step.
- It runs on one lane only (the property is Helm-version-dependent, not CNI-dependent), matching how `chart_upgrade_check` and `chart_reinstall_check` are gated in [`e2e-reusable.yml`](../../../.github/workflows/e2e-reusable.yml).
- The values migration path is covered too, or explicitly noted as out of scope: a released release whose values set a key HEAD removed must fail *at render* with the migration message, not midway through applying.

## Why this is worth a gate rather than a checklist item

The failure is invisible to every local signal. `helm template`, `helm lint`, `kubeconform`, `make check` and the full e2e suite all pass on a chart that no existing release can upgrade to, because none of them has an older release to upgrade *from*.
A reviewer would have to know Helm's `crds/` rule and think to ask.
That is exactly the shape that belongs in CI rather than in a human's memory.

Prior art for the harness: [`chart-upgrade-check.sh`](../../../scripts/e2e/chart-upgrade-check.sh) already captures a live release's values, mutates the chart, upgrades, and restores — most of the scaffolding this needs exists there.
