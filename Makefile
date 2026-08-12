# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.
# Run `make` (or `make help`) for the list of available targets.

# Pin the recipe shell to bash so any bash-only construct in a recipe behaves
# the same on CI (where /bin/sh is dash) and on dev machines (where /bin/sh
# already happens to be bash). Multi-step recipe logic lives in scripts/*.sh
# (shellcheck-covered); recipes here stay thin target→script wiring.
SHELL := /bin/bash

REPO_ROOT := $(shell git rev-parse --show-toplevel)
ACTIONLINT     := $(REPO_ROOT)/.build/actionlint
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
GINKGO         := $(REPO_ROOT)/.build/ginkgo
GOLANGCI_LINT  := $(REPO_ROOT)/.build/golangci-lint
GOVULNCHECK    := $(REPO_ROOT)/.build/govulncheck
CRD_REF_DOCS   := $(REPO_ROOT)/.build/crd-ref-docs
MDREFLOW       := $(REPO_ROOT)/.build/mdreflow
COSIGN         := $(REPO_ROOT)/.build/cosign
# COSIGN_VERSION pins the cosign release used to verify published signatures.
# Keep in step with the `cosign-release` pinned in .github/workflows/publish.yml
# so a local `make verify-release` uses the same verifier the publish run signed
# with. Bump deliberately (see docs/operations/release.md).
COSIGN_VERSION ?= v2.5.2

KIND_CLUSTER  ?= actions-gateway-e2e
# KIND_CONFIG defaults to the 2-worker config so all test suites work out of the box.
# Override with test/kind-config-1worker.yaml if you only need the standard suite and want a faster cluster.
KIND_CONFIG   ?= test/kind-config-2worker.yaml
# KIND_NODE_IMAGE pins the node image (and thus the cluster's K8s version) when set.
# Left empty here so local runs use the installed kind's default; CI sets it to a
# digest-pinned kindest/node so the image can be cached and reused across runs.
KIND_NODE_IMAGE ?=
# KIND_CNI selects the cluster CNI: kindnet (kind's default) or calico.
# `make e2e-cluster KIND_CNI=calico` builds the egress-enforcing profile used to
# observe the NetworkPolicy runtime negatives (Q7b) — kindnet's
# kube-network-policies does not drop egress traffic, so the negative e2e specs
# skip themselves on a kindnet cluster. CALICO_VERSION pins the Calico release.
KIND_CNI       ?= kindnet
CALICO_VERSION ?= v3.31.5
GIT_SHA       := $(shell git rev-parse --short HEAD)

# Local OCI registry that kind nodes pull from. scripts/e2e/kind-with-registry.sh
# runs a registry:2 container on REGISTRY_PORT and wires each kind node's
# containerd to resolve IMAGE_REGISTRY/* against it. All four e2e image tags
# are SHA-suffixed so kubelet's IfNotPresent cache cannot serve a stale image
# when the same tag is rebuilt.
REGISTRY_NAME  ?= kind-registry
REGISTRY_PORT  ?= 5000
# 127.0.0.1, not localhost: the registry is published IPv4-only, so a pusher
# that resolves localhost to IPv6 [::1] first fails intermittently. This string
# is also the containerd mirror key kind nodes resolve (scripts/e2e/kind-with-registry.sh).
IMAGE_REGISTRY ?= 127.0.0.1:$(REGISTRY_PORT)
GMC_IMG        ?= $(IMAGE_REGISTRY)/gmc:e2e-$(GIT_SHA)
AGC_IMG        ?= $(IMAGE_REGISTRY)/agc:e2e-$(GIT_SHA)
PROXY_IMG      ?= $(IMAGE_REGISTRY)/proxy:e2e-$(GIT_SHA)
FAKEGITHUB_IMG ?= $(IMAGE_REGISTRY)/fakegithub:e2e-$(GIT_SHA)
WORKER_IMG     ?= $(IMAGE_REGISTRY)/worker:e2e-$(GIT_SHA)
WRAPPER_IMG    ?= $(IMAGE_REGISTRY)/wrapper:e2e-$(GIT_SHA)

.DEFAULT_GOAL := help

# Every target declares its own `.PHONY` immediately above its rule. A bulk
# block used to restate 53 of them here as well, which is why adding a gate meant
# editing the list in two places and why concurrent gate-adding branches all
# conflicted on this hunk (Q649). `gate-lists-check` fails if a name comes back.

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: all
all: generate build test ## Generate, build, and test all modules

# The one-command pre-review gate. Run this before requesting review or opening a
# PR: gofmt + golangci-lint, STATUS.md format lint, shellcheck over scripts/, and
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
# STATUS.md format slip should not wait out the unit suite to tell you. They go
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
# reason STATUS_GATES exists below. `gate-lists-check` reconciles the recipe,
# the .PHONY declarations and the doc pointer against them.
CHECK_FAST_GATES := lint-backlog status-isolation-check roadmap-check \
                    plan-index-check no-plan-refs-check \
                    go-version-check license-header-check conflict-markers-check \
                    v2-api-sync-check path-filters-check gate-lists-check shellcheck \
                    errexit-prologue-check \
                    actionlint uses-pinned-check chart-crds-check chart-rbac-check chart-webhook-check \
                    codegen-check api-reference-check scripts-test claude-usage-test \
                    doc-links release-pins-check em-dash-check page-density-check \
                    script-docs-check semver-floor-sources-check template-library-check \
                    md-reflow-check
CHECK_HEAVY_GATES := build-tags-check lint cover-check

.PHONY: check
check: ## Fast pre-review gate — `make list-gates` names every gate it runs and what each covers
	scripts/ci/run-parallel.sh $(foreach gate,$(CHECK_FAST_GATES),"$(gate):$(MAKE) $(gate)")
	$(MAKE) build-tags-check
	$(MAKE) lint
	$(MAKE) cover-check
	@# Advisory, not a gate: the fast check deliberately omits the dependency-drift
	@# gates (vendor-check/tidy-check/license-notices run in CI). This reminds you to
	@# run `make vendor-sync` when a change touches dep files. Never fails the build.
	@scripts/ci/check-dep-advisory.sh

.PHONY: list-gates
list-gates: ## List every gate `make check` runs, in order, with what each one covers
	@scripts/ci/gate-list.sh --list --fast '$(CHECK_FAST_GATES)' --heavy '$(CHECK_HEAVY_GATES)'

# Keep the gate list from acquiring a second copy. The recipe above still names
# the heavy phases line by line (one $(MAKE) each, so a `make -j check` cannot
# overlap them), so this asserts those lines match CHECK_HEAVY_GATES, that the
# fast phase runs nothing beyond the CHECK_FAST_GATES fan-out, that every gate is
# a documented .PHONY target, that no target is declared .PHONY twice, that
# SCRIPTS_TESTS names exactly the scripts/**/*-test.sh files on disk, and that
# testing.md still points at the list targets rather than re-transcribing them.
.PHONY: gate-lists-check
gate-lists-check: ## Fail when `make check`'s gate and suite lists disagree with their derived consumers
	scripts/ci/gate-list.sh --check --fast '$(CHECK_FAST_GATES)' --heavy '$(CHECK_HEAVY_GATES)' --status '$(STATUS_GATES)' --suites '$(SCRIPTS_TESTS)'

# The complete set of gates a docs/STATUS.md-only change can fail, so a backlog
# edit can be verified in seconds instead of waiting out the full `make check`:
#   lint-backlog          the format rules
#   status-isolation-check a commit on this branch carries the backlog plus something else
#   roadmap-check         a row changed table or vanished while a roadmap bullet still names it
#   plan-index-check      the last Queue row citing a plan went away, so archival is owed
#   conflict-markers-check a marker survived an Edit-based conflict resolution
#   doc-links             a #QN anchor or plan link broke while rows moved
#   em-dash-check         a Notes cell pushed the file over its baseline ceiling
#   page-density-check    an admonition run in the prose above the tables
# status-isolation-check reads the branch's commits rather than its diff, which
# is why it belongs here rather than only in CI: the fast path exists for the
# hurried resolve-and-push, which is exactly when an --amend lands on the wrong
# HEAD (Q652). It is git-only and costs milliseconds.
# Every entry is also in CHECK_FAST_GATES, so this is a strict subset of `make
# check` and never a second opinion. Completeness is the half that had no
# enforcement: em-dash-check scans `*.md` and page-density-check `docs/*.md`, both
# had been missing since they were written, and this comment called the list
# complete anyway (Q749). `gate-lists-check` now derives the answer from the
# pathspec each gate's script hands git, so a new docs-wide gate cannot omit
# itself. The list lives as a variable, and the docs point at the target rather
# than transcribing it, because a hand-copied list is what drifted first:
# docs/development/maintaining-backlog.md named three of them and called that the
# complete set, so a `docs/STATUS.md` change that parked a row shipped a PR red
# on roadmap-check.
STATUS_GATES := lint-backlog status-isolation-check roadmap-check plan-index-check \
                 conflict-markers-check doc-links em-dash-check page-density-check

.PHONY: status-gates
status-gates: ## Every gate a docs/STATUS.md-only change can fail — the seconds-long verify for a backlog edit
	scripts/ci/run-parallel.sh $(foreach gate,$(STATUS_GATES),"$(gate):$(MAKE) $(gate)")

# Markdown link + anchor integrity gate (Q52). scripts/docs/check-doc-links.sh walks
# every tracked, non-vendored Markdown file and fails on dead relative file
# links or `#anchors` that match no GitHub heading slug / explicit <a id>. The
# dedicated doc-links.yml CI workflow runs this same target, so local and CI
# verdicts match.
.PHONY: doc-links
doc-links: ## Fail on broken relative links / heading anchors in tracked Markdown
	scripts/docs/check-doc-links.sh

# Em-dash density gate (Q654). documentation-standards.md rations the em-dash
# and names a threshold, and nothing enforced it. The counter reads the parsed
# document, so code, headings, link text and raw HTML are excluded — a raw
# `grep -o` counts all four and is why the rule stayed unmeasurable. The tree is
# above the rule today, so the gate ratchets against per-file ceilings in
# scripts/docs/em-dash-baseline.txt; Q650 is the cleanup that empties them.
.PHONY: em-dash-check
em-dash-check: ## Fail when a doc gains em-dashes above its baseline, or a new doc is over the density rule
	scripts/docs/check-em-dash.sh

.PHONY: page-density-check
page-density-check: ## Fail on an admonition wall, or a stat tile saying the same thing on two pages
	scripts/docs/check-page-density.sh

# scripts/README.md coverage gate (Q688). That page is the only map from a
# script to the gate that runs it, and listing the sixteen *-test.sh files that
# had drifted off it fixes the day rather than the week — so the gate is the
# deliverable. Mention detection reads the parsed document, so a filename in a
# fenced example is an illustration rather than an entry.
.PHONY: script-docs-check
script-docs-check: ## Fail when a script under scripts/ has no scripts/README.md entry
	scripts/docs/check-script-docs.sh

.PHONY: em-dash-baseline
em-dash-baseline: ## Re-record the per-file em-dash ceilings; the diff is what the cleanup cleared
	scripts/docs/check-em-dash.sh --write

.PHONY: em-dash-report
em-dash-report: ## Print every doc's em-dash density, worst first — the worklist for the cleanup
	scripts/docs/check-em-dash.sh --report

# The install/upgrade pages transcribe the chart version, image tag, and
# release-notes URL by hand, and nothing bumped them: v1.3.0 shipped with
# upgrade.md and gitops.md still telling operators to install 1.2.0 (Q638). The
# pin-bearing file set and the two exemptions live in the script header.
.PHONY: release-pins-check
release-pins-check: ## Fail when an install/upgrade page pins a release older than the newest stable tag
	scripts/docs/check-release-pins.sh

# The semver floor reads which paths ship from publish.yml's image matrix, the
# Dockerfile stages behind it, and `go list -deps` over the resulting builds —
# so a new image or chart is picked up with no list to maintain. What is NOT
# derivable is a release asset built by a script (the gag-migrate CLI), and that
# declaration is what can outlive the pipeline. This gate is that one seam; the
# floor itself is a report, for the reasons in the devtool's package comment.
.PHONY: semver-floor-sources-check
semver-floor-sources-check: ## Fail when publish.yml grows a release artifact the semver floor's surface derivation misses
	scripts/release/semver-floor.sh --check-sources

.PHONY: semver-floor
semver-floor: ## Report the minimum semver bump the merged work already requires (never fails)
	scripts/release/semver-floor.sh

# Release notes are the one doc whose links are all absolute — they point into
# the versioned site — and check-doc-links skips external URLs by design, so
# nothing resolved them (Q636). The oracle is a local `mkdocs build`, so this
# stays out of `make check`: the fast gate has no business provisioning a venv.
# The doc-links.yml CI workflow runs it, and the script builds site/ if absent.
.PHONY: release-links-check
release-links-check: ## Resolve release-note links into the versioned site against a local site/ build
	scripts/docs/check-release-links.sh

# Enforce the "all go modules use the same Go version" rule (Q68). The two
# go.work.gen files feed `make manifests` via GOWORK= and have silently drifted
# off the repo `go` directive before, breaking code generation. This asserts the
# `go` directive matches across go.work, every go.mod, and every go.work.gen.
.PHONY: go-version-check
go-version-check: ## Assert a single `go` directive across go.work / go.mod / go.work.gen
	scripts/go/check-go-version.sh

# Forbid the scaffolded per-file Apache license header in first-party Go source
# (Q331). The root LICENSE is canonical; the codegen boilerplate.go.txt sources
# are empty so regeneration adds none. Vendored trees keep their headers.
.PHONY: license-header-check
license-header-check: ## Fail if any first-party .go file carries a per-file Apache license header
	scripts/go/check-no-license-headers.sh

.PHONY: conflict-markers-check
conflict-markers-check: ## Fail if any tracked, non-vendored file contains a leftover merge-conflict marker line (Q379)
	scripts/ci/check-conflict-markers.sh

# The shipped runner template library (deploy/templates/) may contain only what
# CI exercises (Q554). A golden template is an implicit validation claim, so an
# entry nothing runs is indistinguishable from a validated one and the library
# rots one plausible addition at a time. Reconciles the shipped set against the
# dogfood e2e overlays that consume it, both directions, and holds every patch
# against a ClusterRunnerTemplate to JSON 6902 — kustomize has no CRD schema, so
# a strategic merge there silently replaces lists wholesale at exit 0.
.PHONY: template-library-check
template-library-check: ## Fail if deploy/templates/ ships an entry no dogfood e2e overlay exercises, or an overlay patches one unsafely (Q554)
	scripts/ci/check-template-library.sh

# Assert every file api/v2alpha1 and api/v2beta1 share stays byte-identical except
# the differences an API version is entitled to — its package clause and the
# storageversion marker (Q345, widened in Q374). Most of what sits beside the
# versioned types is identical by contract, and a one-sided edit breaks the
# storage/hub conversion silently. Files that genuinely differ per version are named
# in the script's EXEMPT list with a reason; everything else is covered by default,
# including files added after this gate landed.
.PHONY: v2-api-sync-check
v2-api-sync-check: ## Fail if a shared api/v2alpha1 + api/v2beta1 file diverges (beyond the package/storageversion lines)
	scripts/go/check-v2-api-sync.sh

# Compile and vet the build-tagged Go files no other fast gate builds (Q404).
# `make lint` and `make test` both use the DEFAULT tag set, so the integration
# (envtest), e2e, and load packages are invisible to them and a compile break
# there only surfaces on CI's path-gated heavy tiers. This vets the workspace
# with every first-party tag enabled (no envtest assets, no cluster, no test
# execution) and fails if a NEW tag appears that its list does not cover.
.PHONY: build-tags-check
build-tags-check: ## Fail if a build-tagged (integration/e2e/load) Go file does not compile or vet clean
	scripts/go/go-vet-tags.sh

# Reconcile CI's hand-maintained `dorny/paths-filter` lists with `go.work` and
# with the paths they name (Q429). A filter that omits a directory makes its gate
# report green by SKIPPING rather than passing, which is how api- and
# scaleset-only changes reached main without meeting the envtest, e2e, or security
# tiers (Q400). Fails if a workspace module is missing from a filter that gates
# whole-workspace work, if a filter is not classified as workspace-covering or
# narrow-by-design, or if a pattern points at a path that no longer exists.
.PHONY: path-filters-check
path-filters-check: ## Fail if a CI path filter misses a go.work module or names a path that no longer exists
	scripts/ci/check-path-filters.sh

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
                 agent/claude-piped-gate-hook-test \
                 agent/foreground-guard-patterns-test \
                 agent/pr-requeue-eligible-test agent/record-launch-test \
                 agent/pr-mergeability-watch-test \
                 agent/qos-cluster-probe-test agent/validate-throttle-test \
                 ci/check-conflict-markers-test ci/check-dep-advisory-test \
                 ci/check-path-filters-test ci/dependabot-rebase-stale-test \
                 ci/gate-list-test ci/shellcheck-scripts-test \
                 ci/check-errexit-prologue-test \
                 ci/check-uses-pinned-test ci/run-parallel-test \
                 ci/check-template-library-test \
                 docs/backlog-metrics-test docs/check-doc-links-test \
                 docs/check-em-dash-test docs/check-page-density-test \
                 docs/check-release-links-test \
                 docs/check-release-pins-test \
                 docs/check-roadmap-test docs/check-no-plan-refs-in-code-test \
                 docs/check-script-docs-test \
                 docs/alloc-queue-id-test docs/check-status-isolation-test \
                 docs/find-duplicate-rows-test \
                 docs/git-merge-plan-index-test docs/git-merge-status-test \
                 docs/lint-backlog-test \
                 docs/release-gates-hook-test docs/release-version-hook-test \
                 docs/source-links-hook-test \
                 dogfood/validate-release-test dogfood/pool-test dogfood/workers-test \
                 dogfood/start-test dogfood/e2e-start-test dogfood/e2e-stop-test \
                 dogfood/delete-test dogfood/e2e-run-watch-test \
                 dogfood/release-status-test dogfood/release-sentinel-test \
                 dogfood/lease-test \
                 e2e/e2e-github-cleanup-test e2e/e2e-report-summary-test \
                 e2e/progress-watch-test e2e/validate-cluster-test \
                 fetch/download-verified-test fetch/pull-image-with-retry-test \
                 go/check-codegen-drift-test go/check-v2-api-sync-test \
                 go/coverage-test go/go-lint-scope-test go/go-test-run-filter-test \
                 go/go-test-integration-test \
                 go/go-vet-tags-test go/go-work-tidy-test \
                 release/download-cosign-test release/release-delta-test \
                 release/verify-release-test \
                 updatecli/latest-cluster-autoscaler-patch-test

.PHONY: scripts-test
scripts-test: ## Run every scripts/ behavioural suite; `make list-script-tests` names them
	scripts/ci/run-parallel.sh $(foreach suite,$(SCRIPTS_TESTS),"$(notdir $(suite)):scripts/$(suite).sh")

.PHONY: list-script-tests
list-script-tests: ## List every scripts/ suite `make scripts-test` runs, grouped by directory
	@scripts/ci/gate-list.sh --list-suites --suites '$(SCRIPTS_TESTS)'

# The claude-usage/ Python suite (Q437). That module is the committed record of
# the project's Claude Code usage, and its merge rule is what guarantees a re-run
# can never revise an already-recorded day downward (the transcripts it reads
# get archived) or collapse two machines' shares of one day. The module matches
# no other path filter in .github/workflows/ — not Go code, not a shell script —
# so until this gate existed the suite ran only when someone remembered.
# stdlib-only: no venv, no pip install (those are for make_charts.py's
# matplotlib/numpy). It also byte-compiles the module, which is the only gate
# covering make_charts.py — that script has no tests and imports matplotlib, so
# nothing else here even parses it. Part of `check` and the CI
# `claude-usage-test` job.
.PHONY: claude-usage-test
claude-usage-test: ## Byte-compile claude-usage/ and run its Python unit tests (usage-snapshot merge semantics)
	scripts/agent/claude-usage-test.sh

# Install the tracked git hooks for this clone by pointing core.hooksPath at the
# in-repo .githooks/ directory. The path is relative, so it resolves correctly in
# the main checkout and every linked worktree. Run once after cloning (scripts/dev/setup.sh
# does this for you). Bypass a single commit with `git commit --no-verify`.
.PHONY: hooks
hooks: ## Install the tracked git hooks (sets core.hooksPath to .githooks)
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath -> .githooks (fast gofmt + STATUS.md gate on commit)"

# Install the two Markdown merge drivers for this clone: docs/STATUS.md by
# backlog row ID, docs/plan/README.md by plan path. .gitattributes already routes
# both files, but git refuses to let a tracked file define a driver's command
# (that would be remote code execution on clone), so the config half is per-clone
# and opt-in. Until it is installed the attributes name undefined drivers and git
# uses its built-in three-way merge — the pre-driver behaviour. Run once after
# cloning (scripts/dev/setup.sh does it for you); repo-local, never --global, and
# shared with every linked worktree.
.PHONY: merge-driver
merge-driver: ## Install the docs/STATUS.md and docs/plan/README.md merge drivers (conflicts resolve by row key)
	scripts/docs/git-merge-status.sh --install
	scripts/docs/git-merge-plan-index.sh --install

# Diagnose the local toolchain: report which required/e2e/extended CLI tools are
# missing or installed-but-not-on-PATH, with per-OS install and PATH-fix hints.
# Runnable without the vendored tool binaries, so it works on a fresh clone.
.PHONY: doctor
doctor: ## Check required CLI tools are installed and on PATH (install/PATH-fix hints for any missing)
	scripts/ci/check-tools.sh

# Each module's `generate` is `manifests deepcopy`, so this regenerates the
# manifests too — it supersets `make manifests` below, and there is no second
# controller-gen pass to pay for. That contract is load-bearing and used to be
# broken: cmd/gmc's `generate` was deepcopy-only, so the root target skipped the
# manifests of the one module whose CRD embeds another module's types — the
# exact miss behind Q440 (Q458). A module whose `generate` stops covering its
# `manifests` silently reopens that hole; `make codegen-check` is what notices.
.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests and DeepCopy methods (all modules)
	$(MAKE) -C api generate
	$(MAKE) -C cmd/gmc generate
	$(MAKE) -C cmd/agc generate

# The manifests half alone, for a change that alters no Go type — adding or
# removing a +kubebuilder:rbac / +kubebuilder:webhook marker. Prefer `make
# generate` when a type changed: `make codegen-check` regenerates BOTH halves for
# these same three modules and diffs them against the committed copies, so this
# target alone no longer makes that gate pass after a type change.
.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC/webhook manifests for all modules (the `codegen-check` remedy; `generate` supersets this)
	$(MAKE) -C api manifests
	$(MAKE) -C cmd/gmc manifests
	$(MAKE) -C cmd/agc manifests

# Fail if committed controller-gen output — CRD/RBAC/webhook manifests, or a
# zz_generated.deepcopy.go — is stale relative to the Go types behind it (Q440,
# Q477). Nothing ran `make generate` on a contributor's behalf, and for manifests
# the gap is worst ACROSS modules: cmd/gmc's ActionsGateway CRD embeds AGC types,
# so a doc comment edited in cmd/agc/api reaches the GMC manifest only when
# someone runs the GMC's own manifests target — #793 edited one and the GMC CRD
# never caught up, handing every later GMC contributor that hunk as diff noise.
# The DeepCopy half was left out on the reasoning that missing DeepCopy fails to
# compile; it does not for an ADDED type, and ClusterCapacity (#917) shipped with
# none, so ActionsGateway.DeepCopy() aliased the informer cache (Q477).
# The script regenerates into a scratch tree, so it never mutates the working
# tree. Fast: six controller-gen runs plus one ~30 MB tree copy, ~4s.
.PHONY: codegen-check
codegen-check: $(CONTROLLER_GEN) ## Fail if committed CRD/RBAC/webhook manifests or zz_generated.deepcopy.go drifted from the Go types controller-gen generates them from (Q440, Q477)
	CONTROLLER_GEN=$(CONTROLLER_GEN) scripts/go/check-codegen-drift.sh

# The published API reference (Q632). crd-ref-docs reads the same doc comments
# and validation markers controller-gen turns into the CRD schemas, so the page
# cannot describe a field the API does not have — but only while it is
# regenerated, which is what api-reference-check (in `make check`) enforces.
# Scope and the deprecated-version decision: docs/development/code-generation.md.
.PHONY: api-reference
api-reference: $(CRD_REF_DOCS) ## Regenerate docs/reference/api.md from the api/v2beta1 kubebuilder markers
	CRD_REF_DOCS=$(CRD_REF_DOCS) scripts/docs/gen-api-reference.sh

.PHONY: api-reference-check
api-reference-check: $(CRD_REF_DOCS) ## Fail if docs/reference/api.md drifted from the api/v2beta1 Go types it is generated from (Q632)
	CRD_REF_DOCS=$(CRD_REF_DOCS) scripts/docs/gen-api-reference.sh --check

.PHONY: build
build: build-agc build-gmc build-probe build-proxy ## Build all binaries into .build/

.PHONY: build-agc
build-agc: ## Build the AGC binary
	go build -C cmd/agc -o ../../.build/agc .

.PHONY: build-gmc
build-gmc: ## Build the GMC binary
	go build -C cmd/gmc/cmd -o ../../../.build/gmc .

.PHONY: build-migrate
build-migrate: ## Build the gag-migrate one-shot v1->v2 migration CLI into .build/
	go build -C cmd/gmc -o ../../.build/gag-migrate ./migrate

.PHONY: build-probe
build-probe: ## Build the probe binary
	go build -C cmd/probe -o ../../.build/probe .

.PHONY: build-proxy
build-proxy: ## Build the proxy binary
	go build -C cmd/proxy -o ../../.build/proxy .

# Regenerate the published broker-compatibility report (Q191) from the compat
# suite. The suite itself runs as a normal unit test (in `make check`); a golden
# test (TestReportInSync) fails if docs/development/broker-compatibility.md drifts
# from what the suite produces — run this to bring it back in sync.
.PHONY: compat-report
compat-report: ## Regenerate docs/development/broker-compatibility.md from the broker-compat suite
	COMPAT_WRITE_REPORT=1 go test -C cmd/probe -run TestReportInSync ./compat/...

# Local preview of the docs/marketing site. The script provisions an isolated
# venv from the pinned requirements-docs.txt (never touching host Python) into
# the gitignored .venv-docs/, reused across runs. python3 is the only host
# prerequisite. Full context: docs/development/website.md.
.PHONY: docs-serve
docs-serve: ## Live-reload the docs/marketing site at http://localhost:8000 (isolated venv)
	scripts/docs/docs-preview.sh serve

# Builds both publication scopes under --strict, so mkdocs' link/anchor
# validation (Q560) gives the same verdict here as pages.yml's PR gate.
.PHONY: docs-build
docs-build: ## Build + strict-validate both docs site scopes (site/, site-dev/)
	scripts/docs/docs-preview.sh build

# The heavy phases (test: one workspace-wide `go test`; lint: a per-module
# loop) live in scripts/go/go-test.sh and
# scripts/go/go-lint.sh, which apply the local auto-throttle themselves
# (scripts/agent/local-throttle.sh: parallelism cap + low-priority QoS prefix on an
# interactive GUI dev shell; no-op on CI/headless — rationale in that script's
# header). V=1 (or VERBOSE=1) streams `go test` output live (-v) for debugging
# a slow or hanging test; make exports command-line variables to recipe
# environments, so `make test V=1` (and `make check V=1`) reach the script.
#
# RUN='TestName' narrows to matching tests, the same spelling the integration
# and e2e targets take. A value matching nothing fails the run rather than
# reporting a green no-op — see the script's header (Q680).
.PHONY: test
test: ## Run unit tests for all modules (RUN='TestName' narrows; V=1 streams output live for debugging a hang)
	scripts/go/go-test.sh

# The race-detector unit gate, run by the `unit-test` CI job. -race instruments
# the concurrency core (agentpool, listener/mux, broker, token) that plain
# `go test` never exercises for data races, at a ~2-10× CPU/memory/I/O cost.
# It is a SEPARATE target from `test`, not folded into it: `make test`/`make
# check` stay the fast local loop, and this heavier run is opt-in locally (like
# `make vulncheck`) so the default dev gate doesn't become an unthrottled `-race`
# run — the same throttle prefix/parallelism cap as `test` applies here, so a
# local invocation on a GUI dev machine stays desktop-safe, while on CI (where
# the throttle is a no-op) it runs at full speed. -timeout is bumped to 5m to
# absorb the instrumentation slowdown.
.PHONY: test-race
test-race: ## Run unit tests under the race detector (the CI unit gate; RUN='TestName' narrows; throttled locally, full speed on CI)
	scripts/go/go-test.sh --race

# --- Test-coverage measurement + no-regression ratchet ---------------------
# scripts/go/coverage.sh measures per-module unit-test coverage (the same per-module
# `go test` the workspace requires — never a repo-root `./...`), filters out
# generated/wiring code, and gates against the recorded floor in
# coverage-baseline.txt. Like `make test`, the script applies the local throttle
# prefix so a run on a GUI dev machine stays desktop-safe; on CI it is a no-op.
# We gate by a no-regression ratchet, not an absolute percentage — see
# docs/development/testing.md and docs/plan/release-1.0.md §F.
.PHONY: cover
cover: ## Report per-module unit-test coverage (writes nothing)
	scripts/go/coverage.sh report

.PHONY: cover-update
cover-update: ## Re-record the coverage baseline floor (coverage-baseline.txt)
	scripts/go/coverage.sh update

.PHONY: cover-check
cover-check: ## Fail if any module drops below its recorded coverage floor (the CI gate)
	scripts/go/coverage.sh check

.PHONY: test-integration
test-integration: ## Run envtest-backed integration tests for cmd/agc and cmd/gmc
	$(MAKE) -C cmd/agc test-integration
	$(MAKE) -C cmd/gmc test-integration

# The in-process AGC load harness (Q13) pins the headline capacity claim —
# thousands of virtual runner sessions per AGC, each costing one re-registration
# per job (Q114). It needs no cluster or credentials; see cmd/agc/test/load.
.PHONY: load-test-quick
load-test-quick: ## Load smoke: 1,000 concurrent virtual sessions, short window (~1 min)
	$(MAKE) -C cmd/agc load-test-quick

.PHONY: load-test-full
load-test-full: ## Load acceptance: 1,000 concurrent virtual sessions, realistic hold, writes a report
	$(MAKE) -C cmd/agc load-test-full

.PHONY: mem-profile
mem-profile: ## Isolate AGC-only per-session memory (Q181): 1,000 parked sessions, in-process transport, no broker stub
	$(MAKE) -C cmd/agc mem-profile

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run gofmt (all modules) + golangci-lint, change-scoped locally to modules affected vs origin/main (LINT_ALL=1 or CI = full sweep; includes govet)
	GOLANGCI_LINT=$(GOLANGCI_LINT) scripts/go/go-lint.sh

.PHONY: lint-backlog
lint-backlog: ## Enforce backlog format rules on docs/STATUS.md (vendored from the backlog skill)
	scripts/docs/lint-backlog.sh

# The pre-commit hook refuses to *stage* docs/STATUS.md next to another file,
# but it reads the index rather than the commit that index produces, so an
# --amend onto a code commit slips past it. This reads the commits the branch
# adds (Q652).
.PHONY: status-isolation-check
status-isolation-check: ## Fail when a commit on this branch mixes docs/STATUS.md with any other file
	scripts/docs/check-status-isolation.sh

# IDs come from a ref claim, not from a counter line in STATUS.md: a shared
# mutable counter conflicted by construction whenever two sessions filed a row
# (Q382). Every path through this target claims — PEEK=1 reported the next free
# ID and reserved nothing, which is that same counter behind a flag (Q656).
#
# TITLE is mandatory, because this target is the one chokepoint every filed row
# passes through and the near-duplicate search needs text (Q639). It travels via
# a target-scoped environment variable rather than an argument so the recipe
# shell never re-parses the title's backticks, quotes or apostrophes — the
# characters these titles are full of. Several IDs at once, or a title carrying
# a literal `$` or `#`: call scripts/docs/alloc-queue-id.sh with one argument
# per title.
#
# TARGET is the link the row's Item cell will carry, when it is already known:
# two rows about one defect point at one file even when they word it
# differently, so an exact match lowers the bar the text has to clear.
.PHONY: queue-id
queue-id: export QUEUE_ID_TITLE = $(TITLE)
queue-id: export QUEUE_ID_TARGET = $(TARGET)
queue-id: ## Search the backlog for near-duplicates, then allocate a Q-ID (make queue-id TITLE="..." [TARGET=path])
	@scripts/docs/alloc-queue-id.sh $${QUEUE_ID_TARGET:+--target "$$QUEUE_ID_TARGET"} "$$QUEUE_ID_TITLE"

# The public roadmap and the backlog drift apart silently — a 2026-07-25 audit
# found six of seven "near-term" items already shipped. Because done Queue rows
# are deleted, a roadmap bullet naming a Q-ID that STATUS.md no longer has is an
# exact signal the work shipped. Rationale + the annotation format are in the
# script header.
.PHONY: roadmap-check
roadmap-check: ## Fail when docs/roadmap.md names backlog rows that shipped or moved tables
	scripts/docs/check-roadmap.sh

# Catches the "closed plan never archived" drift that makes docs/plan/README.md
# read as stale. Rationale + the ⓘ exemption live in the script header.
.PHONY: plan-index-check
plan-index-check: ## Assert active plans in docs/plan/README.md are still STATUS-referenced (else archive them)
	scripts/docs/check-plan-index.sh

# Keeps plan archival a docs-only operation: code that path-links a plan would
# force a code edit (and heavy CI) when that plan is archived. Rationale in the
# script header.
.PHONY: no-plan-refs-check
no-plan-refs-check: ## Assert Go code and shell/workflow comments don't reference plan-doc paths (cite durable docs / Q-IDs instead)
	scripts/docs/check-no-plan-refs-in-code.sh

# Without this gate, standalone helper scripts ship unlinted: the `actionlint`
# target below covers only the inline workflow `run:` blocks. Glob, version pin,
# and rationale live in the script header.
.PHONY: shellcheck
shellcheck: ## Shellcheck every present scripts/*.sh — tracked or untracked-and-not-gitignored (recursive; matches the CI shellcheck gate)
	scripts/ci/shellcheck-scripts.sh

# `set -e` does not reach inside a command substitution, so `x="$$(f)"` runs f
# past its own failure and yields the status of f's last command — a gate that
# checked nothing, reporting success (Q733; Q670 is the same hole reached
# without an injected fault). shellcheck has no check for it, and its SC2155
# covers only the `local x="$$(f)"` half. Rationale and the exemption for
# sourced lib/ files live in the script header.
.PHONY: errexit-prologue-check
errexit-prologue-check: ## Fail if a scripts/ script declares `set -euo pipefail` without `shopt -s inherit_errexit` (Q733)
	scripts/ci/check-errexit-prologue.sh

# The workflow-side half of the shellcheck gate above (Q579). Three docs credited
# actionlint with keeping `uses:` and inline `run:` blocks clean while no gate ran
# it, so a workflow-only PR shipped unlinted. actionlint builds from the vendored
# tools/ module — no new host dependency — and delegates run: blocks to the
# shellcheck already required for `make check`; the script fails when that is
# missing rather than skipping the half silently.
.PHONY: actionlint
actionlint: $(ACTIONLINT) ## Lint .github/workflows/** with actionlint (schema, uses:, expressions + shellcheck over inline run: blocks)
	ACTIONLINT=$(ACTIONLINT) scripts/ci/actionlint-workflows.sh

# What actionlint above does NOT do (Q644): it asserts a `uses:` ref is present
# and well formed, never that it is a commit SHA. Measured against v1.7.12,
# `actions/checkout@v4` and `@main` both exit 0. A tag is mutable by whoever owns
# the action, so that gap is arbitrary third-party code running against the job
# token. Rules and exempt shapes: devtools/ci/usespin's package comment.
.PHONY: uses-pinned-check
uses-pinned-check: ## Assert every workflow/action `uses:` is a 40-hex SHA with a version comment (tags are mutable)
	scripts/ci/check-uses-pinned.sh

.PHONY: queue-unblock
queue-unblock: ## List Queue items blocked by ID=<id> (e.g. make queue-unblock ID=Q12; bare 12 also accepted)
	@if [ -z "$(ID)" ]; then echo "Usage: make queue-unblock ID=<id>" >&2; exit 1; fi
	@scripts/docs/queue-unblock.sh $(ID)

# Consolidated third-party license attribution. scripts/release/gen-third-party-notices.sh
# concatenates every vendored module's LICENSE/NOTICE/COPYING text into the
# committed THIRD-PARTY-NOTICES file, which each production Dockerfile COPYs into
# /licenses/ to satisfy the reproduce-the-notice clauses of the bundled deps
# (Apache-2.0 §4(d), MIT/BSD). It reads only the committed, version-pinned
# vendor/ tree (offline, deterministic). Generate-and-commit so the content is
# reviewable in the diff; `-check` is the CI drift gate (license-notices.yml).
.PHONY: third-party-notices
third-party-notices: ## Regenerate THIRD-PARTY-NOTICES from the committed vendor/ tree
	scripts/release/gen-third-party-notices.sh

.PHONY: third-party-notices-check
third-party-notices-check: ## Fail if THIRD-PARTY-NOTICES is stale vs vendor/ (CI drift gate)
	scripts/release/gen-third-party-notices.sh --check

# Supply-chain integrity gate for the committed vendor trees. `-mod=vendor` only
# checks modules.txt consistency, never that the vendored source matches go.sum;
# this re-vendors (re-fetching modules verified against go.sum) and fails on any
# diff, so a tampered vendor/ edit can't ship into the signed release images
# (Q126). Runs the network re-fetch, so it stays out of the fast `make check`
# gate and runs as its own CI job (unit-test.yml vendor-check).
.PHONY: vendor-check
vendor-check: ## Fail if vendor/ + tools/vendor/ drift from go.sum (CI supply-chain gate)
	scripts/go/vendor-check.sh

# Tidiness gate for the workspace module files (Q94). `go mod tidy` is the
# canonical normaliser for go.mod/go.sum; a non-canonical committed go.sum makes
# the documented tidy flow re-add the /go.mod hash rows, so contributors revert
# spurious diffs. This re-runs the tidy flow (go-work-tidy.sh + go work sync) and
# fails on any go.mod/go.sum/go.work.sum drift. Sibling of vendor-check (Q126):
# this makes the module files canonical, vendor-check makes vendor/ match them.
# Like vendor-check it can need network on a cold cache, so it stays out of the
# fast `make check` gate and runs as its own CI job (unit-test.yml tidy-check).
.PHONY: tidy-check
tidy-check: ## Fail if any go.mod/go.sum/go.work.sum is not tidy (CI tidiness gate)
	scripts/go/go-tidy-check.sh

# One-shot remedy for the three drift gates above. Runs the full "Changing
# dependencies" flow — tidy + go work sync + re-vendor (workspace + tools) +
# regenerate THIRD-PARTY-NOTICES — mutating the working tree so the committed
# go.mod/go.sum, vendor/, and THIRD-PARTY-NOTICES line back up. It is the fix a
# human runs after a dependency change, and what the dependabot-go-sync workflow
# runs to auto-repair a Dependabot Go bump (Q111), which can't run `go work
# vendor` itself. No-ops cleanly when nothing drifted.
.PHONY: vendor-sync
vendor-sync: ## Re-sync module files + vendor trees + THIRD-PARTY-NOTICES (the dependency-change / Dependabot remedy)
	scripts/go/vendor-sync.sh

##@ Security

# The security gates are scripted (scripts/{go-vulncheck,trivy-scan,
# polaris-scan,manifest-validate}.sh) and mirror their CI jobs exactly so local
# and CI verdicts match. Parameters, defaults, and rationale live in each
# script's header; all are env-overridable, and make exports command-line
# variables, so e.g. `make trivy-scan TRIVY_SEVERITY=CRITICAL` or
# `make manifest-validate MANIFEST_K8S_VERSION=1.31.0` reach the script.

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Run govulncheck across all workspace modules (matches the CI govulncheck gate)
	GOVULNCHECK=$(GOVULNCHECK) scripts/security/go-vulncheck.sh

.PHONY: trivy-scan
trivy-scan: ## Build each image locally and scan it with trivy (requires trivy + docker on PATH; matches the CI trivy gate)
	scripts/security/trivy-scan.sh

.PHONY: polaris-scan
polaris-scan: ## Render the Helm chart and audit its Kubernetes posture with polaris (gates on danger findings; requires helm + polaris on PATH; matches the CI polaris gate)
	scripts/security/polaris-scan.sh

.PHONY: chart-crds
chart-crds: ## Regenerate the Helm chart CRD templates from the controller-gen sources (single source of truth, Q73/Q142)
	scripts/manifest/sync-chart-crds.sh

.PHONY: chart-crds-check
chart-crds-check: ## Fail if the chart CRD templates drifted from their sources, or the GMC-bundled RunnerGroup CRD drifted from the AGC copy (Q73)
	scripts/manifest/sync-chart-crds.sh --check

.PHONY: chart-rbac
chart-rbac: ## Regenerate the Helm chart manager-role rules fragment from the controller-gen source (single source of truth, Q142)
	scripts/manifest/sync-chart-rbac.sh

.PHONY: chart-rbac-check
chart-rbac-check: ## Fail if the chart manager-role rules fragment drifted from cmd/gmc/config/rbac/role.yaml (Q142)
	scripts/manifest/sync-chart-rbac.sh --check

.PHONY: chart-webhook
chart-webhook: ## Regenerate the Helm chart validating-webhook template from the controller-gen source (single source of truth, Q143)
	scripts/manifest/sync-chart-webhook.sh

.PHONY: chart-webhook-check
chart-webhook-check: ## Fail if the chart webhook template drifted from cmd/gmc/config/webhook/manifests.yaml (Q143)
	scripts/manifest/sync-chart-webhook.sh --check

.PHONY: manifest-validate
manifest-validate: ## Validate the static install manifests + Helm chart (yamllint + kubeconform + helm lint; requires yamllint, kubeconform, helm on PATH; matches the CI manifest-validate gate)
	scripts/manifest/sync-chart-crds.sh --check
	scripts/manifest/sync-chart-rbac.sh --check
	scripts/manifest/sync-chart-webhook.sh --check
	scripts/manifest/manifest-validate.sh

##@ Operations

# Pre-install cluster preflight (Q184). Validates the target cluster can uphold
# tenant isolation BEFORE `helm install`: CNI NetworkPolicy enforcement (the
# critical one — kindnet silently voids it = hard fail), Kubernetes >= 1.30,
# cert-manager, and metrics-server. Detection-based (no workloads scheduled), so
# it is safe to run against a fresh cluster — the metrics-server check retries
# within a bounded budget, since a just-created cluster's addon is still
# converging when preflight runs (Q397). KUBECTL/VALIDATE_STRICT env-override the
# binary and warning strictness, VALIDATE_METRICS_* the retry budgets (see the
# script header). Operators run this as the required first install step
# (docs/operations/install.md).
.PHONY: validate-cluster
validate-cluster: ## Preflight the target cluster before install (CNI enforcement, K8s>=1.30, cert-manager, metrics-server)
	scripts/e2e/validate-cluster.sh

##@ e2e

.PHONY: e2e-up
e2e-up: e2e-cluster e2e-images e2e ## One-shot: create cluster, build+push images, run all e2e suites

.PHONY: e2e-registry
e2e-registry: ## Start just the local OCI registry (no-op if already running)
	REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) \
		scripts/e2e/start-registry.sh

.PHONY: e2e-cluster
e2e-cluster: ## Create the local kind cluster + registry (no-op if both exist)
	KIND_CLUSTER=$(KIND_CLUSTER) KIND_CONFIG=$(KIND_CONFIG) \
		REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) \
		KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) \
		KIND_CNI=$(KIND_CNI) CALICO_VERSION=$(CALICO_VERSION) \
		scripts/e2e/kind-with-registry.sh

.PHONY: apply-cert-manager
apply-cert-manager: ## Apply cert-manager manifests (version defined in cmd/gmc/Makefile)
	$(MAKE) -C cmd/gmc apply-cert-manager

.PHONY: wait-cert-manager
wait-cert-manager: ## Wait for cert-manager deployments to be Available
	$(MAKE) -C cmd/gmc wait-cert-manager

.PHONY: install-cert-manager
install-cert-manager: ## Apply cert-manager and wait for it to be ready
	$(MAKE) -C cmd/gmc install-cert-manager

# Q444 uninstall/reinstall check — a CI gate (e2e-reusable.yml) as of Q492. It
# ran as a reproduction tool while Q444 was open; the fix moved the guard's
# paramKind off a core type, and this is what keeps the cycle passing. Every
# other test path starts from a cluster that has never had the chart installed,
# so nothing else exercises `helm uninstall` + reinstall. Run it against a
# cluster with the release already installed (after the e2e suite under
# E2E_SKIP_TEARDOWN, or a manual `make deploy`).
# See docs/plan/archive/q444-vap-param-resolution.md.
.PHONY: chart-reinstall-check
chart-reinstall-check: ## Verify the chart survives a helm uninstall/reinstall cycle (needs the release installed)
	KIND_CLUSTER=$(KIND_CLUSTER) scripts/e2e/chart-reinstall-check.sh

# Q475 — the day-2 `helm upgrade` gate. `make deploy` runs `helm upgrade
# --install`, but never against a prior release, so upgrade over a LIVE release
# was untested; in particular nothing proved that CRD field changes still reach
# an existing install (they only do because the CRDs ship under templates/crds/,
# not the chart-root crds/ Helm never upgrades). Unlike chart-reinstall-check
# this one IS wired into CI (e2e-reusable.yml, after the suite). Run it against a
# cluster with the release already installed (after the e2e suite under
# E2E_SKIP_TEARDOWN, or a manual `make deploy`).
.PHONY: chart-upgrade-check
chart-upgrade-check: ## Verify `helm upgrade` delivers chart + CRD changes to a live release (needs the release installed)
	KIND_CLUSTER=$(KIND_CLUSTER) scripts/e2e/chart-upgrade-check.sh

# Q507 — the released-chart upgrade gate. chart-upgrade-check proves HEAD's
# chart upgrades to a copy of itself; nothing proved the chart an operator is
# actually RUNNING upgrades to HEAD, and Q492's CRD placement broke every
# v1.2.0 upgrade while CI stayed green. This one pulls the last released chart
# from GHCR (highest stable v* tag on origin), installs it, and upgrades to
# HEAD along the documented path. Wired into CI (e2e-reusable.yml) LAST among
# the chart checks — it replaces the live release. Run it against a cluster
# with the release already installed (after the e2e suite under
# E2E_SKIP_TEARDOWN, or a manual `make deploy`).
.PHONY: chart-released-upgrade-check
chart-released-upgrade-check: ## Verify the last released chart upgrades to HEAD's chart (needs the release installed)
	KIND_CLUSTER=$(KIND_CLUSTER) scripts/e2e/chart-released-upgrade-check.sh

.PHONY: e2e-cluster-delete
e2e-cluster-delete: ## Delete the local e2e kind cluster (no-op if it does not exist)
	@if kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then \
		echo "==> deleting kind cluster $(KIND_CLUSTER)"; \
		kind delete cluster --name $(KIND_CLUSTER); \
	else \
		echo "==> kind cluster $(KIND_CLUSTER) does not exist"; \
	fi

.PHONY: e2e-registry-delete
e2e-registry-delete: ## Stop and remove the local OCI registry container
	@if docker inspect -f '{{.State.Running}}' $(REGISTRY_NAME) >/dev/null 2>&1; then \
		echo "==> removing registry container $(REGISTRY_NAME)"; \
		docker rm -f $(REGISTRY_NAME) >/dev/null; \
	else \
		echo "==> registry container $(REGISTRY_NAME) does not exist"; \
	fi

# e2e-images builds and pushes all four images in parallel via docker-bake.hcl.
# Bake runs them concurrently bounded by the slowest target instead of summing
# four sequential `docker build` calls. Pushing to the local registry IS the
# load step — kind nodes pull from there on demand. GIT_SHA and IMAGE_REGISTRY
# parameterize the SHA-suffixed tags (see the registry block above).
BAKE = GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) docker buildx bake --file docker-bake.hcl

.PHONY: e2e-images
e2e-images: ## Build and push all four e2e images in parallel via docker buildx bake
	$(BAKE)

.PHONY: docker-build-gmc
docker-build-gmc: ## Build and push only the GMC image (bake target `gmc`)
	$(BAKE) gmc

.PHONY: docker-build-agc
docker-build-agc: ## Build and push only the AGC image (bake target `agc`)
	$(BAKE) agc

.PHONY: docker-build-proxy
docker-build-proxy: ## Build and push only the egress proxy image (bake target `proxy`)
	$(BAKE) proxy

.PHONY: docker-build-fakegithub
docker-build-fakegithub: ## Build and push only the fakegithub image (bake target `fakegithub`)
	$(BAKE) fakegithub

# --procs 4: moderate parallelism tuned for the standard suite on a GitHub
# Actions runner; --procs 8 caused burst scheduling failures.
# E2E_GMC_HPA_PDB and E2E_GMC_Resilience are marked Serial in the test code so
# Ginkgo runs them after all parallel specs complete — no separate invocation or
# label-based split needed for cluster isolation.
#
# SUITE=single-node|multi-node|live-github filters to a subset for local iteration;
# unset runs all specs.
# single-node maps to --label-filter '!multi-node' (tests that run on a 1-worker cluster).
#
# live-github maps to the github-real container, and is the ONLY correct way to run it.
# That container's BeforeAll strips the GMC's AGC_EXTRA_* fakegithub overrides
# cluster-wide and holds them off for its whole duration, so every fakegithub-backed
# spec that stands up a tenant in that window gets an AGC pointed at real GitHub and
# times out unable to register a session. Measured 2026-08-03: five such specs failed
# that way in one full-suite run while the GMC carried AGC_EXTRA_GITHUB_ORG_URL alone.
#
# live-github-scaleset narrows that to the one scale-set spec. It is last in an Ordered
# container, so Ginkgo skips it whenever any spec ahead of it fails — this runs it alone
# once the container is known green, instead of re-paying ~55 minutes to reach it.
SUITE ?=
_SUITE_FILTER = $(if $(filter single-node,$(SUITE)),!multi-node,$(if $(filter multi-node,$(SUITE)),multi-node,$(if $(filter live-github,$(SUITE)),github-real,$(if $(filter live-github-scaleset,$(SUITE)),scaleset-live,))))

# SUITE picks a labelled subset; RUN picks specs *within* whatever survives it,
# by regex over the spec's full text (ginkgo --focus). The two compose, and RUN
# is the same spelling `make test` and the integration targets take, so one
# habit narrows every tier (Q679). Quote a value with spaces:
# RUN='provisions a worker pod'.
RUN ?=

# The suite appends spec start/end events here and scripts/e2e/progress-watch.sh
# renders them into the periodic heartbeat (Q608). The watcher runs BESIDE ginkgo
# rather than inside it because Ginkgo intercepts spec stdout for the duration of
# each spec — the suite cannot narrate its own progress, only write a file
# something else reads. Set empty to disable both halves.
E2E_PROGRESS_FILE ?= $(REPO_ROOT)/tmp/e2e-progress.jsonl

# Whole-suite budget, not a per-spec one: Ginkgo interrupts the running spec when it
# elapses and skips everything after it, so an under-set value reads as a spec failure
# rather than as a budget problem. 30m fits the parallel fake-GitHub suite with room to
# spare. It does not fit live-GitHub, whose container is Ordered and therefore serial:
# its specs wait on real GitHub concluding jobs, which alone is ~10 minutes twice over.
# Measured 2026-08-03 — a 30m run was interrupted in the sixth of seven live specs, so
# the seventh never ran. Raise it with SUITE=live-github rather than for everyone.
E2E_TIMEOUT ?= $(if $(filter live-github live-github-scaleset,$(SUITE)),90m,30m)

# E2E_PROGRESS_FILE sits with the other env vars INSIDE the recipe, after the
# `cd`: a `VAR=x cd dir && cmd` prefix scopes VAR to the `cd` alone, so the
# suite would silently emit nothing.
#
# --fail-on-empty is unconditional rather than tied to RUN, because every filter
# on this target has the same failure shape: ginkgo exits 0 when a --focus or a
# --label-filter selects no spec, so a typo reports a green e2e run. ./test/e2e
# holds one suite and a full run always has specs, so this can only fire on a
# filter that missed.
_GINKGO_RUN = cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
	GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) WORKER_IMG=$(WORKER_IMG) WRAPPER_IMG=$(WRAPPER_IMG) \
	E2E_PROGRESS_FILE=$(E2E_PROGRESS_FILE) \
	$(GINKGO) run --tags e2e --timeout $(E2E_TIMEOUT) --github-output --poll-progress-after 30s --fail-on-empty

# The JUnit report lives under the repo-local tmp/ (gitignored), not /tmp:
# host-wide temp is shared across worktrees/sessions (concurrent runs collide)
# and sits outside the workspace, where sandboxed tooling can't read it back
# when diagnosing a failed run.
#
# The watcher is killed on every exit path — the trap covers a failed suite and
# a Ctrl-C alike — and the recipe still exits with ginkgo's status.
.PHONY: e2e
e2e: $(GINKGO) ## Run e2e tests; SUITE=single-node|multi-node|live-github selects a subset, RUN='regex' narrows to matching specs, both unset runs all
	@mkdir -p $(REPO_ROOT)/tmp
	@: >$(E2E_PROGRESS_FILE)
	E2E_PROGRESS_FILE=$(E2E_PROGRESS_FILE) scripts/e2e/progress-watch.sh & \
	watcher=$$!; \
	trap 'kill -TERM $$watcher 2>/dev/null; wait $$watcher 2>/dev/null; true' EXIT INT TERM; \
	$(_GINKGO_RUN) $(if $(_SUITE_FILTER),--label-filter '$(_SUITE_FILTER)',) \
		$(if $(RUN),--focus '$(RUN)',) \
		--procs 6 --junit-report $(REPO_ROOT)/tmp/e2e-report.xml ./test/e2e/...

.PHONY: e2e-clean
e2e-clean: e2e-cluster-delete e2e-registry-delete ## Tear down the e2e cluster and registry, and delete .build/
	rm -rf .build

# The GitHub-side counterpart to e2e-clean: a live-GitHub run killed with `kill -9`
# skips its AfterAll and strands runner registrations on the fixture repo that no
# cluster teardown can reach. The suite's preflight refuses to start while they are
# there. Destructive against real GitHub — the script confirms before acting, and
# --dry-run reports without changing anything (ARGS='--dry-run').
.PHONY: e2e-github-cleanup
e2e-github-cleanup: ## Clear stranded live-GitHub runners/runs from the fixture repo (ARGS='--dry-run' to preview)
	scripts/e2e/e2e-github-cleanup.sh $(ARGS)

##@ Live autoscaler

# The capacity gate's elastic-cluster signal recognizes cluster-autoscaler by
# upstream's Event reasons, pinned in our unit table from recorded samples. A
# reword upstream fails open — the gate just stops closing, silently, on every
# elastic cluster — so nothing in the fast tiers can notice. These targets run
# the matcher against a REAL cluster-autoscaler (kwok cloud provider, fake nodes)
# in its own throwaway kind cluster, which is what makes that drift a failure.
# Separate from the e2e cluster on purpose: a live autoscaler creating and
# deleting nodes underneath the e2e suite would perturb every spec in it.
# Detail: docs/development/testing.md § The live-autoscaler drift gate.
AUTOSCALER_CLUSTER ?= gag-autoscaler

.PHONY: autoscaler-cluster
autoscaler-cluster: ## Create the kind cluster running a real cluster-autoscaler on kwok nodes (no-op if it exists)
	AUTOSCALER_CLUSTER=$(AUTOSCALER_CLUSTER) KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) \
		scripts/e2e/autoscaler-cluster.sh

.PHONY: test-autoscaler
test-autoscaler: ## Assert the autoscaler matcher against a live cluster-autoscaler's events (needs autoscaler-cluster)
	AUTOSCALER_CLUSTER=$(AUTOSCALER_CLUSTER) $(MAKE) -C cmd/agc test-autoscaler

.PHONY: autoscaler-cluster-delete
autoscaler-cluster-delete: ## Delete the live-autoscaler kind cluster (no-op if it does not exist)
	@if kind get clusters 2>/dev/null | grep -qx $(AUTOSCALER_CLUSTER); then \
		echo "==> deleting kind cluster $(AUTOSCALER_CLUSTER)"; \
		kind delete cluster --name $(AUTOSCALER_CLUSTER); \
	else \
		echo "==> kind cluster $(AUTOSCALER_CLUSTER) does not exist"; \
	fi

# The Karpenter arm of the same drift gate (Q479). Karpenter's declination
# shares kube-scheduler's reason string, so this is the arm whose correctness
# is the reporter discrimination — and the one a vocabulary or attribution
# change upstream would disable most silently. Its own cluster for the same
# reason as above, and separate from the autoscaler one because two autoscalers
# fighting over the same pending pods would make both harnesses flaky.
KARPENTER_CLUSTER ?= gag-karpenter

.PHONY: karpenter-cluster
karpenter-cluster: ## Create the kind cluster running a real Karpenter (kwok provider) on fake nodes (no-op if it exists)
	KARPENTER_CLUSTER=$(KARPENTER_CLUSTER) KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) \
		scripts/e2e/karpenter-cluster.sh

.PHONY: test-karpenter
test-karpenter: ## Assert the autoscaler matcher against a live Karpenter's events (needs karpenter-cluster)
	KARPENTER_CLUSTER=$(KARPENTER_CLUSTER) $(MAKE) -C cmd/agc test-karpenter

.PHONY: karpenter-cluster-delete
karpenter-cluster-delete: ## Delete the live-Karpenter kind cluster (no-op if it does not exist)
	@if kind get clusters 2>/dev/null | grep -qx $(KARPENTER_CLUSTER); then \
		echo "==> deleting kind cluster $(KARPENTER_CLUSTER)"; \
		kind delete cluster --name $(KARPENTER_CLUSTER); \
	else \
		echo "==> kind cluster $(KARPENTER_CLUSTER) does not exist"; \
	fi

##@ Tools

.PHONY: tools
tools: $(ACTIONLINT) $(CONTROLLER_GEN) $(CRD_REF_DOCS) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GINKGO) $(GOLANGCI_LINT) $(GOVULNCHECK) $(MDREFLOW) ## Build all vendored build tools into .build/

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Build golangci-lint into .build/

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Build govulncheck into .build/

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST) ## Build setup-envtest into .build/

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Build ginkgo into .build/

# Sentence-per-line prose formatting. Configured by .mdreflow.yaml at the repo
# root; mdreflow always excludes vendor/, so the tracked vendored Markdown is
# untouched. md-reflow-check runs in CHECK_FAST_GATES (~1s for the tree) because
# an unenforced wrap convention decays on the next hand-wrapped paragraph.
# It converts ~99.9% of prose: a paragraph is skipped by design when a link's
# text or destination is left open at a line end. Never hand-wrap inside a link:
# that wedges the paragraph permanently. See documentation-standards.md.
.PHONY: md-reflow
md-reflow: $(MDREFLOW) ## Reflow tracked Markdown prose to one sentence per line
	$(MDREFLOW) .

# status-scope: none — .mdreflow.yaml excludes docs/STATUS.md (table-shaped, and
# reflow would fight its merge driver), so a backlog edit cannot fail this gate
# and STATUS_GATES leaves it out. The declaration is what gate-lists-check reads:
# this recipe runs a Go tool rather than a scripts/ file, so its file set is not
# derivable from a git pathspec.
.PHONY: md-reflow-check
md-reflow-check: $(MDREFLOW) ## Report Markdown that is not sentence-per-line; writes nothing
	$(MDREFLOW) --check .

.PHONY: md-reflow-diff
md-reflow-diff: $(MDREFLOW) ## Print the reflow diff without writing
	$(MDREFLOW) --diff .

.PHONY: cosign
cosign: $(COSIGN) ## Download pinned cosign (COSIGN_VERSION) into .build/

.PHONY: verify-release
verify-release: $(COSIGN) ## Verify cosign signatures for a published release: make verify-release VERSION=vX.Y.Z
	@COSIGN=$(COSIGN) scripts/release/verify-release.sh $(VERSION)

# These all build the same way from the vendored tools/ module; only the package
# path differs (the target-specific TOOL_PKG below). ginkgo is the exception: it
# builds from cmd/gmc (workspace on) so it matches the ginkgo version the e2e
# suite imports.
$(ACTIONLINT):     TOOL_PKG := github.com/rhysd/actionlint/cmd/actionlint
$(CONTROLLER_GEN): TOOL_PKG := sigs.k8s.io/controller-tools/cmd/controller-gen
$(CRD_REF_DOCS):   TOOL_PKG := github.com/elastic/crd-ref-docs
$(KUBEBUILDER):    TOOL_PKG := sigs.k8s.io/kubebuilder/v4
$(SETUP_ENVTEST):  TOOL_PKG := sigs.k8s.io/controller-runtime/tools/setup-envtest
$(GOLANGCI_LINT):  TOOL_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
$(GOVULNCHECK):    TOOL_PKG := golang.org/x/vuln/cmd/govulncheck
$(MDREFLOW):       TOOL_PKG := github.com/jbeda/mdreflow/cmd/mdreflow

$(ACTIONLINT) $(CONTROLLER_GEN) $(CRD_REF_DOCS) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GOLANGCI_LINT) $(GOVULNCHECK) $(MDREFLOW):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ $(TOOL_PKG)

$(GINKGO):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/cmd/gmc && go build -o $@ github.com/onsi/ginkgo/v2/ginkgo

# cosign is a non-Go-vendored binary tool (its dependency tree is too large to
# vendor like the kubebuilder-ecosystem tools above), so it is downloaded at a
# pinned version — the same pattern as the shellcheck/kubeconform CI installs.
$(COSIGN):
	scripts/release/download-cosign.sh $@ $(COSIGN_VERSION)
