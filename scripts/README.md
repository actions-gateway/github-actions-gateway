# scripts/

Developer and CI helper scripts, grouped by **blast radius** — which gate consumes the script. Nothing lives at the top level, so "which gate cares about this?" has to be answered when a script is added rather than defaulting to a catch-all. Every CI path filter that names a script is a plain prefix glob over one of these directories, and [`ci/check-path-filters.sh`](ci/check-path-filters.sh) holds them to it (Q571).

| Directory | Holds | Gated by |
|---|---|---|
| [`ci/`](#ci--repo-hygiene-gates) | the repo-hygiene meta-gates and the gate runner | `unit-test.yml`, `conflict-markers.yml` |
| [`go/`](#go--go-build-test-lint-vendor) | build, test, lint, vendor, coverage, codegen drift | `unit-test.yml` |
| [`e2e/`](#e2e--live-cluster-tiers) | cluster bring-up, image bake, chart install checks | `e2e-test.yml`, `e2e-calico.yml`, `autoscaler-drift.yml` |
| [`fetch/`](#fetch--the-download--retry-family) | the download/retry/pre-pull family every tier calls | every heavy tier |
| [`docs/`](#docs--docs-site-backlog-plan) | docs site, backlog, plan and roadmap tooling | `doc-links.yml`, `status-lint.yml`, `plan-hygiene.yml` |
| [`security/`](#security--vulnerability-and-posture-scans) | govulncheck, trivy, polaris | `security-scan.yml` |
| [`manifest/`](#manifest--chart-and-manifest-generation) | chart CRD/RBAC/webhook sync and install-artifact validation | `manifest-validate.yml` |
| [`release/`](#release--publish-sign-report) | publish, sign, notices, release decision support | `publish.yml`, `license-notices.yml` |
| [`agent/`](#agent--claude-code-hooks-and-the-local-throttle) | Claude Code hooks, the local throttle and its instruments | never CI-gating |
| [`dev/`](#dev--developer-only) | post-clone setup, live probes, cloud spikes | never CI-gating |
| [`dogfood/`](dogfood/) | GKE dogfood tenant tooling | `dogfood-*.yml` |
| [`lib/`](lib/) | sourced helpers, no entry points | every group |
| [`updatecli/`](updatecli/) | version-pin resolvers for `updatecli.yml` | `updatecli.yml` |

A `*-test.sh` sits beside its subject, so it inherits its subject's gate: [`docs/source-links-hook-test.sh`](docs/source-links-hook-test.sh) belongs to the docs site and cannot trigger e2e.

All scripts follow the [repo bash conventions](../docs/development/bash-style.md): `set -euo pipefail`, `local` for function variables, `[[ ]]` conditionals, quoted expansions, `trap` cleanup for background processes — see the doc for the full list. Shared helpers (`require_cmd`, `step`/`die`/`gh_curl`, `workspace_modules`, the throttle setup) live in [lib/common.sh](lib/common.sh); the `manifest/sync-chart-{crds,rbac,webhook}.sh` generators share their temp-file/cleanup/dispatch skeleton via [lib/chart-sync.sh](lib/chart-sync.sh). Every script here is linted by `make shellcheck` ([ci/shellcheck-scripts.sh](ci/shellcheck-scripts.sh)) as soon as the file exists — tracked or not — so opting a scratch script out means gitignoring it (write it under the gitignored `tmp/` at the repo root), not merely leaving it untracked.

The root `Makefile` keeps recipes as thin target→script wiring so the logic is shellcheck-covered; parameters are env-overridable and documented in each script's header.

A gate whose logic outgrows shell moves its core to Go in [`devtools/`](../devtools/), the first-party tooling module, keeping the script here as the entry point so the gate map stays in one place. Packages there mirror these directories — `devtools/ci/pathfilters/` backs [`ci/check-path-filters.sh`](ci/check-path-filters.sh). The module is deliberately outside `go.work`; the reasoning and the wiring a new one needs are in [go-workspaces.md](../docs/development/go-workspaces.md#first-party-go-tooling-stays-outside-the-workspace).

## `ci/` — repo-hygiene gates

| Script | Purpose |
|---|---|
| [check-path-filters.sh](ci/check-path-filters.sh) | Reconcile CI's hand-maintained `dorny/paths-filter` lists with `go.work` and with the paths they name — a filter missing a module makes its gate report green by *skipping* (Q400/Q429). Fails on an uncovered workspace module, an unclassified filter, a pattern whose path is gone, or two lanes over one reusable workflow disagreeing about scripts/ (Q571); assertions covered by `check-path-filters-test.sh` under `make scripts-test`. Backs `make path-filters-check` and the CI `path-filters` job. |
| [check-conflict-markers.sh](ci/check-conflict-markers.sh) | Fail on leftover merge-conflict marker lines in any tracked, non-vendored file (Q379 — a stray marker from a rebase resolution once merged to main). Backs `make conflict-markers-check` and the CI `conflict-markers` workflow. |
| [shellcheck-scripts.sh](ci/shellcheck-scripts.sh) | Shellcheck every present `scripts/**/*.sh` — tracked or untracked-and-not-gitignored, recursive (file selection asserted by `shellcheck-scripts-test.sh` under `make scripts-test`). Backs `make shellcheck`. |
| [check-dep-advisory.sh](ci/check-dep-advisory.sh) | Print a one-line reminder when a change touches Go dependency files, and stay silent otherwise — `make check` deliberately omits the three dependency-drift gates, so this is the nudge to run `make vendor-sync`. Advisory: never fails the build. |
| [check-tools.sh](ci/check-tools.sh) | Verify the CLI tools the project needs (required / e2e / extended tiers) are installed and on PATH; for each miss, print a per-OS install command or the exact dir to add to PATH. Cross-platform (brew/apt/url). Backs `make doctor`. Exits nonzero if a required tool is missing. |
| [dependabot-rebase-stale.sh](ci/dependabot-rebase-stale.sh) | Rebase a conflicted Dependabot Go-module PR by **replaying** its version bumps on current `main` (`go get` per bump, then `make vendor-sync`) and force-pushing, never by merging: merging can silently downgrade a module. Backs the [`dependabot-rebase-stale`](../.github/workflows/dependabot-rebase-stale.yml) workflow (Q427); `--dry-run`, `--list`, and `--bumps` for local use. Rationale: [go-workspaces.md](../docs/development/go-workspaces.md#dependabot-go-bumps-are-auto-synced). |
| [run-parallel.sh](ci/run-parallel.sh) | Run multiple commands in parallel with labeled, real-time output. Backs the fan-out in `make check`, `make status-gates` and `make scripts-test`. |

## `go/` — Go build, test, lint, vendor

| Script | Purpose |
|---|---|
| [go-test.sh](go/go-test.sh) | Workspace unit tests in one multi-module `go test` invocation; `--race` for the race-detector gate. Backs `make test` / `make test-race`. |
| [go-lint.sh](go/go-lint.sh) | gofmt check (all modules) + per-module golangci-lint, change-scoped locally to the modules affected vs the origin/main merge-base (`LINT_ALL=1` or CI = full sweep; scoping decision asserted by `go-lint-scope-test.sh` under `make scripts-test`). Backs `make lint` and the CI `lint` job. |
| [go-vet-tags.sh](go/go-vet-tags.sh) | Compile + vet the build-tagged (`integration`/`e2e`/`load`/`autoscaler`) Go files no other fast gate builds, and fail if a new tag appears that its list does not cover (Q404: a tagged-file compile break used to reach only CI's path-gated heavy tiers; both properties asserted by `go-vet-tags-test.sh` under `make scripts-test`). Backs `make build-tags-check` and the CI `lint` job. |
| [coverage.sh](go/coverage.sh) | Per-module unit-test coverage + the no-regression ratchet gate. Backs `make cover`/`cover-update`/`cover-check`. |
| [check-codegen-drift.sh](go/check-codegen-drift.sh) | Regenerate every registered module's CRD/RBAC/webhook manifests into a scratch tree and fail if a committed copy is stale — the GMC's ActionsGateway CRD embeds AGC types, so an edit in `cmd/agc/api` goes stale in the GMC manifest until someone runs the GMC's `manifests` target (Q440). Also fails on an unregistered module with a `manifests:` target, or a registry row that stopped matching its recipe. Backs `make codegen-check` and the CI `lint` job. |
| [check-v2-api-sync.sh](go/check-v2-api-sync.sh) | Fail when the v2alpha1 and v2beta1 API packages drift apart. Backs `make v2-api-sync-check`. |
| [check-go-version.sh](go/check-go-version.sh) | Assert a single `go` directive across `go.work`, every `go.mod`, and every `go.work.gen` (Q68 — the generated workspace files consumed by `make manifests` had drifted). Backs `make go-version-check`. |
| [check-no-license-headers.sh](go/check-no-license-headers.sh) | Forbid the scaffolded per-file Apache license header in first-party Go source (Q331); the root LICENSE is the canonical grant. Backs `make license-header-check`. |
| [vendor-check.sh](go/vendor-check.sh) | Fail if the committed vendor trees drift from `go.sum` — `go build -mod=vendor` only checks `vendor/modules.txt`, never the vendored source against its hashes. Backs `make vendor-check`. |
| [vendor-sync.sh](go/vendor-sync.sh) | Re-sync the workspace module files, vendor trees, and notices in dependency order — the full "Changing dependencies" remedy flow in one shot. Backs `make vendor-sync`. |
| [go-tidy-check.sh](go/go-tidy-check.sh) | Fail if any module's `go.mod`/`go.sum` differs from its tidy-canonical shape. Backs `make tidy-check`. |
| [go-work-tidy.sh](go/go-work-tidy.sh) | Run `go mod tidy` across every module in the Go workspace, leaf-first. See [docs/development/go-workspaces.md](../docs/development/go-workspaces.md). |

## `e2e/` — live-cluster tiers

| Script | Purpose |
|---|---|
| [kind-with-registry.sh](e2e/kind-with-registry.sh) | Idempotent: start a local OCI registry and a `kind` cluster wired to use it. Foundation for cluster-only/fake-GitHub e2e tests — see [docs/development/kind-iteration.md](../docs/development/kind-iteration.md). |
| [start-registry.sh](e2e/start-registry.sh) | Idempotent: start just the local OCI registry container. Backs `make e2e-registry`; also called by kind-with-registry.sh. |
| [bake-with-retry.sh](e2e/bake-with-retry.sh) | `docker buildx bake` with bounded retries — the image bake/push step on the e2e bring-up critical path. |
| [free-runner-disk.sh](e2e/free-runner-disk.sh) | Reclaim disk on a hosted runner before the bake, in the background, so the e2e tier does not die of disk pressure mid-run. |
| [validate-cluster.sh](e2e/validate-cluster.sh) | Pre-install cluster preflight (Q184): assert the target cluster can actually uphold the tenant-isolation guarantees the GMC/AGC depend on — CNI NetworkPolicy *enforcement* above all — before `helm install`, rather than after a silent failure. Backs `make validate-cluster`; helpers asserted by `validate-cluster-test.sh`. |
| [chart-reinstall-check.sh](e2e/chart-reinstall-check.sh) | Cycle `helm uninstall` → reinstall against a cluster already running the release and assert admission still works — the day-two path no fresh-cluster test covers (Q444). Asserts every `paramKind` policy survives the uninstall and no binding does. Backs `make chart-reinstall-check`; a **CI gate** since Q492 moved the guard's `paramKind` off a core type (it ran as a reproducer while the defect was open). |
| [chart-upgrade-check.sh](e2e/chart-upgrade-check.sh) | `helm upgrade` a live release to a chart carrying a synthetic CRD schema field + Deployment annotation, then back, and assert both arrive and are then removed — the day-two path no fresh-cluster test covers (Q475). Fails closed if the CRDs ever move to the chart-root `crds/` Helm never upgrades. Also asserts tenant objects survive by UID and the webhook keeps enforcing. Backs `make chart-upgrade-check`; runs in CI after the e2e suite (kindnet leg only). |
| [chart-released-upgrade-check.sh](e2e/chart-released-upgrade-check.sh) | Install the **last released chart** (the published OCI artifact, pulled from GHCR at the highest stable `v*` tag on origin) and upgrade it to HEAD along the documented path — the transition Q492 broke for every v1.2.0 install while all of CI stayed green. Asserts upgrade-blocking failures happen at render with the documented pre-upgrade message, every chart-root `crds/` CRD lands, and admission works afterwards. Backs `make chart-released-upgrade-check`; runs in CI last among the chart checks (kindnet leg only). |
| [vap-param-informer-check.sh](e2e/vap-param-informer-check.sh) | Reproduce the kube-apiserver VAP param-informer defect (Q444) deterministically, with no chart involved: three arms on one apiserver differing only in whether the `paramKind`'s binding set is emptied and whether the `paramKind` is a core type or a CRD. Arm 3 is the measured evidence for Q492's fix — a CRD `paramKind` survives the exact transition that kills a ConfigMap one. **Disposable clusters only** — it permanently breaks ConfigMap param resolution for that apiserver process, and refuses a non-`kind-` context unless `ALLOW_NON_KIND=1`. Not wired into CI. |
| [e2e-github-cleanup.sh](e2e/e2e-github-cleanup.sh) | Clear live-GitHub e2e state stranded on the fixture repo: deregister the suite's runners and cancel any workflow run still in flight. The GitHub-side counterpart to `make e2e-clean` — a run killed with `kill -9` skips its `AfterAll`, and the registrations it leaves poison the next run (Q511). The suite's `BeforeAll` preflight refuses to start while they are present and names this script. Destructive against real GitHub: confirms first (`ASSUME_YES=1` skips), and `--dry-run` reports without acting. Params: `GITHUB_E2E_ORG`/`GITHUB_E2E_REPO` (required), `GITHUB_E2E_GATEWAY` (default `real-ag`). Backs `make e2e-github-cleanup`; filter asserted by `e2e-github-cleanup-test.sh`. |
| [autoscaler-cluster.sh](e2e/autoscaler-cluster.sh) | Idempotent: a `kind` cluster running a **real** upstream cluster-autoscaler on its kwok cloud provider (fake nodes), so the capacity gate's autoscaler matcher can be asserted against live events instead of recorded samples — a reword upstream fails open and would otherwise go unnoticed (Q474). `CA_VERSION`/`KWOK_VERSION` pin what is installed; manifests in [test/autoscaler/](../test/autoscaler/). Backs `make autoscaler-cluster`; the test is `make test-autoscaler`. See [testing.md](../docs/development/testing.md#the-live-autoscaler-drift-gate). |
| [karpenter-cluster.sh](e2e/karpenter-cluster.sh) | Idempotent: a `kind` cluster running a **real** upstream Karpenter (kwok provider, fake nodes) — the second arm of the same drift gate, and the one whose matcher arm is pure reporter discrimination (Q479). Upstream publishes no image for the kwok provider, so the script clones the `KARPENTER_VERSION` tag and builds it; manifests in [test/karpenter/](../test/karpenter/). Backs `make karpenter-cluster`; the test is `make test-karpenter`. See [testing.md](../docs/development/testing.md#the-live-autoscaler-drift-gate). |

## `fetch/` — the download / retry family

Three entry points by what they retry — an arbitrary command, a `curl`, a `docker pull` — plus the two pre-pull cachers built on the last. Every heavy tier calls at least one, which is why `scripts/fetch/**` appears in the e2e, security-scan, manifest-validate and autoscaler-drift filters.

| Script | Purpose |
|---|---|
| [retry.sh](fetch/retry.sh) | Run an arbitrary command with bounded retries and linear backoff. Used by the publish workflow to absorb transient GHCR errors on idempotent push/sign steps. |
| [download-verified.sh](fetch/download-verified.sh) | `<url> <sha256> <output-path>`: download a pinned release asset with `curl --retry-all-errors` (`DOWNLOAD_RETRIES`/`DOWNLOAD_RETRY_DELAY` env, default 5×2s) and write the output path only once the bytes match the digest. Used by every pinned-binary install: kind, shellcheck, kubeconform, polaris, cosign. |
| [pull-image-with-retry.sh](fetch/pull-image-with-retry.sh) | `docker pull <image-ref>` with bounded retries and exponential, jittered backoff (`PULL_RETRY_ATTEMPTS`/`PULL_RETRY_DELAY`/`PULL_RETRY_MAX_DELAY` env, default 6 attempts, 5s doubling to a 60s cap). Absorbs transient registry timeouts/429s in-step; the jitter keeps concurrent CI callers from retrying in lockstep (Q460). Used by the e2e, security-scan and publish workflows to pre-pull the buildkit builder and mirror the curl/Vault test images. |
| [prepull-image-cached.sh](fetch/prepull-image-cached.sh) | `<image-ref> <cache-dir> <local-tag>`: pre-pull one pinned image into the runner's Docker daemon (retried) and cache it as a tarball, exposing it under a local-only tag; a warm cache `docker load`s the tar without touching the registry. The single-image sibling of prepull-manifest-images.sh — the local tag exists because `docker load` cannot restore a manifest digest, so a digest-pinned ref would never resolve from the cache. Used by the security-scan trivy job's buildkit pre-pull. |
| [prepull-manifest-images.sh](fetch/prepull-manifest-images.sh) | `<name> <manifest-url> <cache-dir>`: extract the image refs a pinned Kubernetes manifest names, pre-pull them into the runner's Docker daemon (retried) and cache as a tarball + `images.txt`; on a warm cache `docker load`s the tar without touching the network. The shared pre-pull half of the e2e Calico/cert-manager/metrics-server caching pattern. |

## `docs/` — docs site, backlog, plan

| Script | Purpose |
|---|---|
| [check-doc-links.sh](docs/check-doc-links.sh) | GitHub-slug-aware Markdown link/anchor checker: fails on dead relative file links or `#anchors` with no matching heading slug / `<a id>`. Backs `make doc-links` and the CI `doc-links` workflow. |
| [docs-preview.sh](docs/docs-preview.sh) | Serve or build the MkDocs site locally. Backs `make docs-serve` / `make docs-build`. |
| [source-links-hook-test.sh](docs/source-links-hook-test.sh) | Unit tests for `hooks/source_links.py`, the MkDocs hook that absolutizes relative links escaping `docs/` against `repo_url` (Q558). |
| [release-version-hook-test.sh](docs/release-version-hook-test.sh) | Unit tests for `hooks/release_version.py`, the MkDocs hook that derives the announce bar's release from the git tags (Q393) — asserted in hermetic throwaway repos so no assumption about the caller's tree leaks in. |
| [lint-backlog.sh](docs/lint-backlog.sh) | Lint `docs/STATUS.md` for backlog format rules (vendored from the backlog skill): no `**Next ID:**` counter, unique IDs + matching anchors, 🔲/🚫-only states, Notes ≤250 chars with the >200-char doc-link rule, Deferred trigger tags. `--staged` (pre-commit mode) also enforces commit isolation. Runs in CI (`unit-test.yml`, `status-lint.yml`), by `make check`, and by the pre-commit hook. |
| [check-roadmap.sh](docs/check-roadmap.sh) | Fail when `docs/roadmap.md` and `docs/STATUS.md` disagree — a row changed table or vanished while a roadmap bullet still names it. Backs `make roadmap-check`. |
| [check-plan-index.sh](docs/check-plan-index.sh) | Fail when the last Queue row citing a plan doc went away without the plan being archived and its `docs/plan/README.md` row updated. Backs `make plan-index-check`. |
| [check-no-plan-refs-in-code.sh](docs/check-no-plan-refs-in-code.sh) | Fail on `docs/plan/` references from Go source — plans are transient, code comments are not. Backs `make no-plan-refs-check`. |
| [git-merge-status.sh](docs/git-merge-status.sh) | Git merge driver for `docs/STATUS.md`: resolves Queue-table conflicts by row ID (deleted on either side → deleted, added on either side → present) and falls back to ordinary conflict markers for anything ambiguous. Row rules live in [lib/merge-status-rows.awk](lib/merge-status-rows.awk); every rule — resolving *and* refusing — is asserted against real three-way merges by `git-merge-status-test.sh` under `make scripts-test`. One-time `make merge-driver` per clone — a no-op until then, since git will not let `.gitattributes` define a driver's command. Rationale: [maintaining-backlog.md](../docs/development/maintaining-backlog.md#the-merge-driver-resolve-queue-rows-by-id-not-by-line-position). |
| [alloc-queue-id.sh](docs/alloc-queue-id.sh) | Allocate a backlog Q-ID by claiming a `refs/queue-ids/QN` ref on the remote, so concurrent sessions never take the same ID. Backs `make queue-id`. Rationale: [queue-id-allocation.md](../docs/development/queue-id-allocation.md). |
| [queue-unblock.sh](docs/queue-unblock.sh) | List `docs/STATUS.md` Queue items blocked on a given ID. Backs `make queue-unblock`. |
| [next-task.sh](docs/next-task.sh) | Print a kickoff prompt (or `--title`) for the top ready 🔲 Queue row in `docs/STATUS.md`, for starting a fresh session on the next task. Vendored from the backlog skill. |
| [backlog-metrics.sh](docs/backlog-metrics.sh) | Replay `docs/STATUS.md` git history into per-item events and flow metrics (throughput, cycle time, prune ratio, aging WIP). Only Queue/Deferred rows count — a Progress-table anchor is not an item (Q509; asserted by `backlog-metrics-test.sh` under `make scripts-test`). Read-only. Vendored from the backlog skill. |

## `security/` — vulnerability and posture scans

| Script | Purpose |
|---|---|
| [go-vulncheck.sh](security/go-vulncheck.sh) | Per-module govulncheck. Backs `make vulncheck` and the CI `govulncheck` job. |
| [trivy-scan.sh](security/trivy-scan.sh) | Build each image locally and scan with trivy. Backs `make trivy-scan`; mirrors the CI `trivy` matrix. |
| [polaris-scan.sh](security/polaris-scan.sh) | Render the Helm chart (digest-pinned) and audit posture with polaris. Backs `make polaris-scan`; mirrors the CI `polaris` job. |

## `manifest/` — chart and manifest generation

| Script | Purpose |
|---|---|
| [manifest-validate.sh](manifest/manifest-validate.sh) | yamllint + kubeconform + helm lint + the fail-closed digest-pinning assertion over the install artifact. Backs `make manifest-validate`; mirrors the CI `validate` job. |
| [sync-chart-crds.sh](manifest/sync-chart-crds.sh) | Regenerate (or `--check`) the chart's CRD manifests from the Go types. Backs `make chart-crds`/`chart-crds-check`. |
| [sync-chart-rbac.sh](manifest/sync-chart-rbac.sh) | Regenerate (or `--check`) the chart's RBAC from the controller markers. Backs `make chart-rbac`/`chart-rbac-check`. |
| [sync-chart-webhook.sh](manifest/sync-chart-webhook.sh) | Regenerate (or `--check`) the chart's webhook configuration from the controller markers. Backs `make chart-webhook`/`chart-webhook-check`. |

## `release/` — publish, sign, report

| Script | Purpose |
|---|---|
| [build-migrate-binaries.sh](release/build-migrate-binaries.sh) | Cross-build the migration binaries a release publishes as assets. |
| [verify-release.sh](release/verify-release.sh) | Verify the cosign signatures of a published release (5 images + chart). Backs `make verify-release`. |
| [download-cosign.sh](release/download-cosign.sh) | Download the pinned cosign release binary for the current platform (pin table + platform resolution; fetch and verify via [fetch/download-verified.sh](fetch/download-verified.sh)). Backs the Makefile's `$(COSIGN)` rule. |
| [gen-third-party-notices.sh](release/gen-third-party-notices.sh) | Regenerate (or `--check`) THIRD-PARTY-NOTICES from the committed vendor/ trees. Backs `make third-party-notices(-check)`. |
| [release-delta.sh](release/release-delta.sh) | Report what has accumulated since the last stable tag — commits by Conventional Commit type with breaking changes called out, Queue rows closed in the window, the API diffstat that is the semver signal, and the operator-facing pages touched. Answers "should a release be scoped at all?"; once one *is* scoped, its plan doc's scope ledger and the `-gate` labels take over. Needs no bookkeeping: Conventional Commits and the delete-on-done Queue's commit history already carry it. Reports, never fails — the triggers that turn it into a decision are in [release.md § When to cut](../docs/operations/release.md#when-to-cut). Derivations asserted by `release-delta-test.sh` under `make scripts-test`. |
| [api-surface-since.sh](release/api-surface-since.sh) | Enumerate the API surface a release would publish for the first time — new wire fields, enum constraints, defaults, condition types/reasons, label keys — between a ref (default: the newest tag) and `HEAD`. The input-gathering half of the [pre-release API review](../docs/development/api-review.md); reports, never fails, because every question the review asks needs a human. Condition and label sections compare value *sets* rather than diff lines, so a refactor that relocates the vocabulary does not read as a hundred new conditions. |
| [operator-caveats-since.sh](release/operator-caveats-since.sh) | Enumerate the operator-facing caveats a release is about to publish — added sections, bold-lead bullets and anything marked `BREAKING` in `docs/operations/upgrade.md` and `troubleshooting.md` — between a ref (default: the newest tag) and `HEAD`. Feeds the [release pre-flight](../docs/operations/release.md#1-pre-flight) decision on whether to curate the Release body, which must happen *before* the tag is pushed. Needs no bookkeeping: the doc-update matrix already requires operator-visible changes to land in those pages. Reports, never fails — a clarification is not a caveat and only a human can tell. |

## `agent/` — Claude Code hooks and the local throttle

Nothing in CI gates on this group; it is tooling for an interactive dev session.

| Script | Purpose |
|---|---|
| [claude-go-throttle-hook.sh](agent/claude-go-throttle-hook.sh) | Claude Code `PreToolUse` hook that rewrites a bare `go build`/`go test` to carry the local-throttle prefix (Q92). Wired in `.claude/settings.json`. |
| [claude-no-subagent-workers-hook.sh](agent/claude-no-subagent-workers-hook.sh) | Claude Code `PreToolUse` hook that asks before a sub-agent spawn that looks like a parallel-dispatch worker — workers must be task chips ([parallel-dispatch.md](../docs/development/parallel-dispatch.md)). |
| [local-throttle.sh](agent/local-throttle.sh) | Detect an interactive GUI dev shell and emit a parallelism cap + low-priority QoS command prefix (empty on CI/headless), so heavy gates stay desktop-safe. Also sizes a [parallel-dispatch](../docs/development/parallel-dispatch.md#concurrency-and-contention) batch (`workers`), which answers on headless shells too. |
| [qos-cluster-probe.sh](agent/qos-cluster-probe.sh) | Measure how much of a Mac's CPU a candidate throttle prefix can actually reach (per-cluster residency × clock), so `local-throttle.sh` sizing is set from data. macOS, needs sudo. |
| [validate-throttle.sh](agent/validate-throttle.sh) | Run a real cold-cache `make check` phase under each candidate throttle prefix while [uijitter.c](agent/uijitter.c) samples desktop scheduling latency, reporting throughput against desktop cost. macOS. |
| [claude-usage-test.sh](agent/claude-usage-test.sh) | Byte-compile `claude-usage/` and run its Python unit tests (Q437) — that module is the committed record of the project's Claude Code usage, and its merge rule is what guarantees a re-run can never revise an already-recorded day downward. Backs `make claude-usage-test`. |

## `dev/` — developer-only

| Script | Purpose |
|---|---|
| [setup.sh](dev/setup.sh) | One-time post-clone setup: initialise Go module dependencies, install the git hooks and the `docs/STATUS.md` merge driver, and verify the build. Re-run after any dependency change. |
| [probe-live-run.sh](dev/probe-live-run.sh) | End-to-end setup and execution of the Milestone 1 wire-protocol probe against a real GitHub App installation. |
| [probe-investigations-cd.sh](dev/probe-investigations-cd.sh) | Runs Milestone 1 Investigations C and D against real GitHub. |
| [kata-node-pool.sh](dev/kata-node-pool.sh) | Provision (or `DRY_RUN=1` print) a GKE Standard node pool with nested virtualization on a nested-virt-capable machine family (n2/n2d/c2/c2d), for the Q226 Kata-on-GKE spike. Params: `PROJECT`/`CLUSTER`/`REGION` (required), `MACHINE_TYPE`/`NODE_POOL`/`NUM_NODES`/… (optional). See [deploy/kata-ci/](../deploy/kata-ci/). |
| [validate-egress-ip.sh](dev/validate-egress-ip.sh) | Live GKE validation of per-tenant egress-IP pinning (Q243, the residual after Q282): stands up a throwaway Standard/DPv2 cluster with two tenant node pools + per-range Cloud NAT, installs the GMC, deploys a `spec.scheduling`-pinned `EgressProxy` per tenant, and asserts each pool egresses from a single distinct, stable NAT IP; tears the infra down on exit. **Billable** — gated behind a dogfood-project guard + confirmation. Params: `PROJECT` (throwaway, required), `GAG_IMAGE_TAG` (required), `ZONE`/`CLUSTER`/`IP_REFLECTOR`/`BUILD_IMAGE`/`KEEP`/`ASSUME_YES` (optional). See [docs/plan/q243-egress-ip-reference-arch.md](../docs/plan/q243-egress-ip-reference-arch.md#re-runnable-live-validation). |

## Per-clone setup

The tracked git hooks live in [`.githooks/`](../.githooks/). Install them with `make hooks` (or [dev/setup.sh](dev/setup.sh), which does it for you); the pre-commit hook runs a sub-second gate (gofmt on staged Go files, plus `docs/lint-backlog.sh --staged` when `docs/STATUS.md` is staged — format rules and the isolated-commit requirement). Bypass a single commit with `git commit --no-verify`.

The `docs/STATUS.md` merge driver is the other per-clone `git config`: `make merge-driver` (also run by [dev/setup.sh](dev/setup.sh)). The committed half is the `merge=backlog` line in [`.gitattributes`](../.gitattributes); git refuses to let a tracked file supply the driver's command, so the config is opt-in per clone and the attribute is a no-op without it. A clone that installed the driver before Q571 has the old `scripts/git-merge-status.sh` path in its config and must re-run `make merge-driver`.
