# gate-lists.mk — the gate and suite lists `make check` derives from.
#
# These live in their own file because they are a registry: every PR that adds
# a gate or a suite appends one entry, so two such PRs collide on adjacent
# lines by construction. .gitattributes routes this file to the `gatelists`
# merge driver, which merges the entries as a set rather than by line position
# (scripts/ci/git-merge-gate-lists.sh, installed per clone by `make
# merge-driver`).
#
# Routing is per file, so the routed file has to be one that is wholly
# driver-owned. Keeping these lists in the Makefile would have routed the whole
# Makefile, sending every ordinary conflict in it through a driver that
# understands only these lists.

# The one-command pre-review gate. Run this before requesting review or opening a
# PR: gofmt + golangci-lint, the backlog store rules, shellcheck over scripts/, and
# the (plain) unit tests — the fast local loop. The CI `unit-test` job runs the
# same unit tests but under the race detector (`make test-race`); that heavier
# run stays out of `check` so the dev gate doesn't become an unthrottled `-race`
# run. A green `make check` covers the lint and unit-test logic; reproduce the
# race gate with `make test-race` when a change touches concurrency. The slower
# security gates (vulncheck, trivy-scan) and the integration/e2e tiers stay
# separate too.
#
# The cheap gates below take no heavy-build slot and are independent of each
# other, so they run concurrently (~50s serial -> ~15s) and report first: a
# backlog format slip should not wait out the unit suite to tell you. They go
# through scripts/ci/run-parallel.sh rather than `make -j` because macOS ships GNU
# make 3.81, which has no `-O` output sync — `-j` would interleave two failing
# gates' output unreadably, while run-parallel.sh labels every line with its
# gate. The heavy phases stay sequential after them: each takes a machine-wide
# slot of its own (serialize_heavy_build), so overlapping them would just queue
# on the semaphore. build-tags-check runs first of the three — a compile break
# should not wait out lint and the suite.
#
# These two variables are the single source of truth for what `make check` runs.
# `make list-gates` renders them with each gate's own `##` description, so the
# docs name that target instead of transcribing the list (Q649) — the same
# reason QUEUE_GATES exists below. `gate-lists-check` reconciles the recipe,
# the .PHONY declarations and the doc pointer against them — and, since Q831,
# `.github/workflows/` too: a gate listed here that no workflow runs is enforced
# by `make check` alone, so it gates nothing on a PR. A gate that is
# deliberately local-only says so with `# ci-scope: none` above its .PHONY.
# Since Q942 the workflow that runs it must also declare `merge_group`, or the
# queue's candidate merge — the commit that carries the merge result — is never
# held to the gate; `# merge-queue-scope: none` is that rule's declaration.
CHECK_FAST_GATES := roadmap-check \
                    plan-index-check no-plan-refs-check \
                    go-version-check license-header-check conflict-markers-check \
                    v2-api-sync-check test-cache-inputs-check \
                    path-filters-check gate-lists-check shellcheck \
                    errexit-prologue-check \
                    actionlint uses-pinned-check cosign-pin-check \
                    publish-digest-check \
                    chart-crds-check chart-rbac-check chart-webhook-check \
                    codegen-check api-reference-check scripts-test claude-usage-test \
                    doc-links release-pins-check em-dash-check page-density-check \
                    script-docs-check queue-rules-check queue-lint \
                    getting-started-check \
                    semver-floor-sources-check template-library-check \
                    md-reflow-check comparison-stamps-check promql-check \
                    metric-tiers-check reason-tiers-check upgrade-toc-check \
                    endpoint-parity-check \
                    release-notes-check \
                    dashboard-render-check \
                    tool-pin-check \
                    release-ladder-check \
                    vendored-skills-check

CHECK_HEAVY_GATES := build-tags-check lint cover-check

# The complete set of gates a docs/queue/-only change can fail, so a backlog
# edit can be verified in seconds instead of waiting out the full `make check`:
#   queue-lint            the store's own format: frontmatter, rank, title cap, a target that no longer resolves
#   queue-rules-check     a flake item vanished, a plan's last item left its index row open, a label is undeclared
#   roadmap-check         an item changed status or vanished while a roadmap bullet still names it
#   plan-index-check      the last item citing a plan went away, so archival is owed
#   conflict-markers-check a marker survived an Edit-based conflict resolution
#   doc-links             an item's target or a plan link broke while items moved
#   em-dash-check         an item's notes pushed the file over its baseline ceiling
#   md-reflow-check       an item body written as one paragraph rather than one sentence per line
#   page-density-check    an admonition run, or a stat tile repeated across pages
#   release-ladder-check  an item revived or re-parked while release-ladder.md still sorts it the old way
# Every entry is also in CHECK_FAST_GATES, so this is a strict subset of `make
# check` and never a second opinion. Completeness is the half that had no
# enforcement: em-dash-check scans `*.md` and page-density-check `docs/*.md`, both
# had been missing since they were written, and this comment called the list
# complete anyway (Q749). `gate-lists-check` now derives the answer from the
# pathspec each gate's script hands git, so a new docs-wide gate cannot omit
# itself. The list lives as a variable, and the docs point at the target rather
# than transcribing it, because a hand-copied list is what drifted first:
# docs/development/maintaining-backlog.md named three of them and called that the
# complete set, so a backlog change that parked an item shipped a PR red on
# roadmap-check.
#
# Three members retired with the table (Q889). lint-backlog and
# status-isolation-check took docs/STATUS.md as their subject rather than as a
# source; queue-drift-check held the table and the store to the same items
# through the migration and has nothing left to compare.
QUEUE_GATES := queue-lint queue-rules-check roadmap-check plan-index-check \
                 conflict-markers-check doc-links em-dash-check md-reflow-check \
                 page-density-check \
                 release-ladder-check

# The gates a prose change can fail, for the same reason QUEUE_GATES exists one
# rung over: they cost seconds each and they are what a docs change trips at the
# very END of a ten-minute `make check`, which is the worst possible moment to
# learn it. CLAUDE.md asked contributors to remember to run them as a set the
# moment prose was written; that instruction failed four times (Q699, the session
# that closed it, Q715, and the Q844 session that added this target), so the set
# becomes one command rather than six things to remember.
#   doc-links            a relative link re-based by a move, or a dangling #QNNN
#   plan-index-check     a plan doc left active with no Queue row citing it
#   no-plan-refs-check   code or a workflow comment citing a plan path that moved
#   em-dash-check        a file pushed over its baseline ceiling, or a new doc over the density rule
#   getting-started-check an install-doc block that lost its gag:verify annotation, or dropped below the executed floor
#   md-reflow-check      prose that is not sentence-per-line
#   page-density-check   an admonition wall, or a stat tile repeated across pages
#   release-pins-check   an install/upgrade page pinning a superseded release
#   release-notes-check  a release note with a duplicate h1, a dead in-page anchor, or a `v`-prefixed chart version
#   upgrade-toc-check    a heading added to upgrade.md that its own index never gained
#   conflict-markers-check a marker survived an Edit-based conflict resolution
#   release-ladder-check an edit to release-ladder.md's punted table or its stated counts
#   roadmap-check        a roadmap or features.md bullet over its word cap, or naming an item that moved
#   comparison-stamps-check an ARC-column verdict in why-gag.md left without a version and date
#   promql-check         an alert renamed in observability-alerting.md or the runbook but not in the rule
#   metric-tiers-check   a metric's tier edited in observability-metrics.md away from what the AGC emits
#   reason-tiers-check   a reason edited in observability-metrics.md or troubleshooting.md the same way
#   api-reference-check  docs/reference/api.md hand-edited away from what controller-gen renders
#   gate-lists-check     testing.md stopping short of citing the list targets it must name
# Every entry is also in CHECK_FAST_GATES, so like QUEUE_GATES this is a strict
# subset of `make check` and never a second opinion. That claim went unchecked
# for the life of the list and was false: release-notes-check was in neither gate
# list, so `make docs-gates` ran a gate `make check` did not, and
# conflict-markers-check scans the whole tree while the list omitted it — the
# Q749 shape one rung over. `gate-lists-check` now holds this list to both
# directions the way it holds QUEUE_GATES (Q920). Since Q930 it also reads the
# subject a gate hardcodes, not only the pathspecs it hands git: every
# page-scoped gate here is written the second way, so the rule had been blind to
# seven of them at once and `make docs-gates` was green on prose edits to nine
# pages `make check` can fail.
DOCS_GATES := doc-links plan-index-check no-plan-refs-check em-dash-check \
              getting-started-check \
              md-reflow-check page-density-check release-pins-check \
              release-notes-check upgrade-toc-check \
              conflict-markers-check \
              release-ladder-check \
              roadmap-check comparison-stamps-check promql-check \
              metric-tiers-check reason-tiers-check \
              api-reference-check gate-lists-check

# Behavioural assertions for the scripts/ tree that shellcheck (a linter) can't
# express — the tags-only release signing-identity regexp (Q124), the
# validate-cluster preflight decision helpers (CNI classification + K8s version
# parsing, Q184, plus the bounded metrics-server retry against faked probes —
# a still-converging addon must not warn, an absent one still must, Q397), the
# dogfood gate's e2e run resolution (an in-flight run must
# not abort the gate after the billable scale-up), the go-lint change-scoping
# decision (which modules a diff makes golangci-lint cover), the build-tag
# coverage guard (a new tag must fail the gate, not silently skip files), that a
# pinned download never writes bytes it did not verify (Q433), the shellcheck
# gate's own file selection (an untracked-but-present script must be linted,
# Q432), the errexit-prologue gate, whose own failure mode is the one it exists
# to catch — a rule that stopped matching would pass every script in silence, so
# both directions are asserted and an empty selection must go red (Q733),
# the dogfood worker-drain gate (an unreadable cluster must never read
# as idle and let a teardown strand worker nodes, Q434), the on-demand e2e
# tenant bring-up (its readiness wait is the bring-up's whole verdict and both
# directions are silent: an undersized system pool leaves the AGC Pending, so a
# healthy cluster reads as a timeout, and a wait whose failure did not abort
# would point repo-wide e2e routing at a tenant that never came up, Q578), and
# the CI path-filter
# gate (a workspace module missing from a filter must fail, since the gate it
# would skip reports green either way, Q429), the image-pull retry schedule
# (exponential, jittered and capped, so concurrent CI callers cannot retry in
# lockstep and an unreachable registry still fails on a bounded budget, Q460),
# the cluster-autoscaler patch resolver updatecli runs unattended (it must stay
# inside the pinned Kubernetes minor and never downgrade, or the weekly bump
# manufactures the very version skew the drift gate exists to catch, Q483),
# the cosign digest resolver beside it (it decides which bytes may become the
# binary that verifies every release, so it has to read them from a checksums
# file whose sigstore signature it has checked, and print nothing when it has
# not — neither half is visible in review of the manifest, Q927),
# the release gate's ownership lease, which is the sole trigger for tearing down
# a prod-classified cluster the running process never scaled up — a live gate or
# a hand-run debugging session read as orphaned deletes work in progress, which
# is worse than the leak it exists to stop (Q640),
# the release-validation status renderer and the sentinel that wakes a session
# from it (both directions are silent: a failure attributed to the wrong phase
# sends a diagnosis the wrong way, and a sentinel that never fires leaves a
# wedged hour-long gate unnoticed — the wake itself is asserted live, against a
# stream that transitions under a running watcher, Q616),
# and the throttle instruments'
# parsers (iostat/powermetrics/vm_stat text -> the numbers a throttle decision
# rests on, Q447 — the measurement paths are macOS-only and one needs root, but
# the parsers are text-to-number and run here). Lightweight pure-bash checks;
# part of `check` and the CI shellcheck job.
#
# The suites are independent and each isolates its own scratch state (mktemp -d,
# or a $$-suffixed dir under tmp/), so they run concurrently — labeled output via
# run-parallel.sh keeps a failure attributable to its suite.
#
# This variable is the single source of truth for what `make scripts-test` runs,
# so adding a suite is one edit here. `make list-script-tests` renders it, which
# is why the target's `##` help names that target instead of enumerating: the
# enumeration it replaced was 1,399 characters that every suite-adding PR
# rewrote — two of them conflicted by construction (#1243 vs #1239) — and it had
# already drifted to 50 names for 55 suites (Q671). `gate-lists-check`
# reconciles this list against the scripts/**/*-test.sh files on disk, both ways.
SCRIPTS_TESTS := agent/claude-go-throttle-hook-test agent/local-throttle-test \
                 agent/foreground-guard-patterns-test \
                 agent/pr-requeue-eligible-test agent/record-launch-test \
                 agent/pr-mergeability-watch-test \
                 agent/qos-cluster-probe-test agent/validate-throttle-test \
                 ci/check-conflict-markers-test ci/check-dep-advisory-test \
                 ci/check-path-filters-test ci/dependabot-rebase-stale-test \
                 ci/gate-list-test ci/shellcheck-scripts-test \
                 ci/check-errexit-prologue-test ci/check-tools-test \
                 ci/git-merge-gate-lists-test \
                 docs/git-merge-script-index-test \
                 ci/check-uses-pinned-test ci/check-cosign-pin-test \
                 ci/check-publish-digest-test \
                 ci/run-parallel-test ci/check-fixture-maintenance-test \
                 ci/check-template-library-test \
                 docs/backlog-metrics-test docs/check-comparison-stamps-test \
                 docs/check-doc-links-test \
                 docs/check-em-dash-test docs/check-page-density-test \
                 docs/check-upgrade-toc-test \
                 docs/check-release-links-test \
                 docs/check-release-pins-test \
                 docs/check-roadmap-test docs/check-no-plan-refs-in-code-test \
                 docs/check-plan-index-test docs/check-script-docs-test \
                 docs/alloc-queue-id-test \
                 docs/find-duplicate-rows-test \
                 docs/git-merge-plan-index-test docs/git-merge-roadmap-test \
                 docs/check-queue-rules-test \
                 docs/queue-unblock-test docs/reconcile-queue-rows-test \
                 docs/queue-test docs/rank-vectors-test \
                 docs/release-gates-hook-test docs/release-version-hook-test \
                 docs/source-links-hook-test \
                 dogfood/validate-release-test dogfood/pool-test dogfood/workers-test \
                 dogfood/nodes-test dogfood/quota-test \
                 dogfood/start-test dogfood/e2e-start-test dogfood/e2e-stop-test \
                 dogfood/delete-test dogfood/e2e-run-watch-test \
                 dogfood/release-status-test dogfood/release-sentinel-test \
                 dogfood/lease-test \
                 e2e/e2e-github-cleanup-test e2e/e2e-report-summary-test \
                 e2e/progress-watch-test e2e/validate-cluster-test \
                 fetch/download-verified-test fetch/pull-image-with-retry-test \
                 manifest/check-promql-test \
                 go/check-codegen-drift-test go/check-v2-api-sync-test \
                 go/check-test-cache-inputs-test \
                 go/coverage-test go/go-lint-scope-test go/go-test-run-filter-test \
                 go/go-test-integration-test \
                 go/go-vet-tags-test go/go-work-tidy-test \
                 release/api-surface-since-test \
                 release/download-cosign-test release/release-delta-test \
                 release/verify-release-test release/verify-published-docs-test \
                 release/check-artifact-unchanged-test release/check-gates-green-test \
                 release/check-candidate-covers-main-test \
                 release/render-release-body-test release/check-release-notes-test \
                 release/check-release-digests-test \
                 updatecli/latest-cluster-autoscaler-patch-test \
                 docs/check-metric-tiers-test docs/check-reason-tiers-test \
                 e2e/check-endpoint-parity-test \
                 manifest/check-dashboard-render-test \
                 ci/check-tool-pins-test \
                 updatecli/cosign-release-sha256-test \
                 docs/check-release-ladder-test \
                 ci/check-vendored-skills-test \
                 docs/doc-blocks-test
