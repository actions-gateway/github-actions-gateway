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
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
GINKGO         := $(REPO_ROOT)/.build/ginkgo
GOLANGCI_LINT  := $(REPO_ROOT)/.build/golangci-lint
GOVULNCHECK    := $(REPO_ROOT)/.build/govulncheck
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

# Local OCI registry that kind nodes pull from. scripts/kind-with-registry.sh
# runs a registry:2 container on REGISTRY_PORT and wires each kind node's
# containerd to resolve IMAGE_REGISTRY/* against it. All four e2e image tags
# are SHA-suffixed so kubelet's IfNotPresent cache cannot serve a stale image
# when the same tag is rebuilt.
REGISTRY_NAME  ?= kind-registry
REGISTRY_PORT  ?= 5000
# 127.0.0.1, not localhost: the registry is published IPv4-only, so a pusher
# that resolves localhost to IPv6 [::1] first fails intermittently. This string
# is also the containerd mirror key kind nodes resolve (scripts/kind-with-registry.sh).
IMAGE_REGISTRY ?= 127.0.0.1:$(REGISTRY_PORT)
GMC_IMG        ?= $(IMAGE_REGISTRY)/gmc:e2e-$(GIT_SHA)
AGC_IMG        ?= $(IMAGE_REGISTRY)/agc:e2e-$(GIT_SHA)
PROXY_IMG      ?= $(IMAGE_REGISTRY)/proxy:e2e-$(GIT_SHA)
FAKEGITHUB_IMG ?= $(IMAGE_REGISTRY)/fakegithub:e2e-$(GIT_SHA)
WORKER_IMG     ?= $(IMAGE_REGISTRY)/worker:e2e-$(GIT_SHA)
WRAPPER_IMG    ?= $(IMAGE_REGISTRY)/wrapper:e2e-$(GIT_SHA)

.DEFAULT_GOAL := help

.PHONY: all check hooks generate build build-agc build-gmc build-migrate build-probe build-proxy test test-race test-integration \
        cover cover-update cover-check tools setup-envtest \
        e2e-registry e2e-cluster e2e-cluster-delete e2e-images e2e e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub \
        ginkgo golangci-lint lint lint-backlog plan-index-check no-plan-refs-check shellcheck queue-unblock queue-id \
        third-party-notices third-party-notices-check vendor-check tidy-check \
        vulncheck govulncheck trivy-scan polaris-scan manifest-validate

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
# through scripts/run-parallel.sh rather than `make -j` because macOS ships GNU
# make 3.81, which has no `-O` output sync — `-j` would interleave two failing
# gates' output unreadably, while run-parallel.sh labels every line with its
# gate. The heavy phases (build-tags-check, lint, cover-check) stay sequential
# after them: each takes a machine-wide slot of its own (serialize_heavy_build),
# so overlapping them would just queue on the semaphore. build-tags-check runs
# first of the three — a compile break should not wait out lint and the suite.
CHECK_FAST_GATES := lint-backlog roadmap-check plan-index-check no-plan-refs-check \
                    go-version-check license-header-check conflict-markers-check \
                    v2-api-sync-check path-filters-check shellcheck chart-crds-check \
                    chart-rbac-check chart-webhook-check scripts-test doc-links

.PHONY: check
check: ## Fast pre-review gate: gofmt + golangci-lint + STATUS.md lint + roadmap/backlog coherence + plan-index/no-plan-refs drift + single-Go-version + no per-file license headers + no leftover conflict markers + v2 API package sync + CI path-filter coverage + build-tagged compile/vet + shellcheck + chart-CRD/RBAC/webhook drift + scripts-test + doc link/anchor check + unit tests with the coverage ratchet (cover-check supersets `make test`; CI also runs tests under -race, see `make test-race`)
	scripts/run-parallel.sh $(foreach gate,$(CHECK_FAST_GATES),"$(gate):$(MAKE) $(gate)")
	$(MAKE) build-tags-check
	$(MAKE) lint
	$(MAKE) cover-check
	@# Advisory, not a gate: the fast check deliberately omits the dependency-drift
	@# gates (vendor-check/tidy-check/license-notices run in CI). This reminds you to
	@# run `make vendor-sync` when a change touches dep files. Never fails the build.
	@scripts/check-dep-advisory.sh

# Markdown link + anchor integrity gate (Q52). scripts/check-doc-links.sh walks
# every tracked, non-vendored Markdown file and fails on dead relative file
# links or `#anchors` that match no GitHub heading slug / explicit <a id>. The
# dedicated doc-links.yml CI workflow runs this same target, so local and CI
# verdicts match.
.PHONY: doc-links
doc-links: ## Fail on broken relative links / heading anchors in tracked Markdown
	scripts/check-doc-links.sh

# Enforce the "all go modules use the same Go version" rule (Q68). The two
# go.work.gen files feed `make manifests` via GOWORK= and have silently drifted
# off the repo `go` directive before, breaking code generation. This asserts the
# `go` directive matches across go.work, every go.mod, and every go.work.gen.
.PHONY: go-version-check
go-version-check: ## Assert a single `go` directive across go.work / go.mod / go.work.gen
	scripts/check-go-version.sh

# Forbid the scaffolded per-file Apache license header in first-party Go source
# (Q331). The root LICENSE is canonical; the codegen boilerplate.go.txt sources
# are empty so regeneration adds none. Vendored trees keep their headers.
.PHONY: license-header-check
license-header-check: ## Fail if any first-party .go file carries a per-file Apache license header
	scripts/check-no-license-headers.sh

.PHONY: conflict-markers-check
conflict-markers-check: ## Fail if any tracked, non-vendored file contains a leftover merge-conflict marker line (Q379)
	scripts/check-conflict-markers.sh

# Assert every file api/v2alpha1 and api/v2beta1 share stays byte-identical except
# the differences an API version is entitled to — its package clause and the
# storageversion marker (Q345, widened in Q374). Most of what sits beside the
# versioned types is identical by contract, and a one-sided edit breaks the
# storage/hub conversion silently. Files that genuinely differ per version are named
# in the script's EXEMPT list with a reason; everything else is covered by default,
# including files added after this gate landed.
.PHONY: v2-api-sync-check
v2-api-sync-check: ## Fail if a shared api/v2alpha1 + api/v2beta1 file diverges (beyond the package/storageversion lines)
	scripts/check-v2-api-sync.sh

# Compile and vet the build-tagged Go files no other fast gate builds (Q404).
# `make lint` and `make test` both use the DEFAULT tag set, so the integration
# (envtest), e2e, and load packages are invisible to them and a compile break
# there only surfaces on CI's path-gated heavy tiers. This vets the workspace
# with every first-party tag enabled (no envtest assets, no cluster, no test
# execution) and fails if a NEW tag appears that its list does not cover.
.PHONY: build-tags-check
build-tags-check: ## Fail if a build-tagged (integration/e2e/load) Go file does not compile or vet clean
	scripts/go-vet-tags.sh

# Reconcile CI's hand-maintained `dorny/paths-filter` lists with `go.work` and
# with the paths they name (Q429). A filter that omits a directory makes its gate
# report green by SKIPPING rather than passing, which is how api- and
# scaleset-only changes reached main without meeting the envtest, e2e, or security
# tiers (Q400). Fails if a workspace module is missing from a filter that gates
# whole-workspace work, if a filter is not classified as workspace-covering or
# narrow-by-design, or if a pattern points at a path that no longer exists.
.PHONY: path-filters-check
path-filters-check: ## Fail if a CI path filter misses a go.work module or names a path that no longer exists
	scripts/check-path-filters.sh

# Behavioural assertions for the scripts/ tree that shellcheck (a linter) can't
# express — the tags-only release signing-identity regexp (Q124), the
# validate-cluster preflight decision helpers (CNI classification + K8s version
# parsing, Q184), the dogfood gate's e2e run resolution (an in-flight run must
# not abort the gate after the billable scale-up), the go-lint change-scoping
# decision (which modules a diff makes golangci-lint cover), the build-tag
# coverage guard (a new tag must fail the gate, not silently skip files), that a
# pinned download never writes bytes it did not verify (Q433), the shellcheck
# gate's own file selection (an untracked-but-present script must be linted,
# Q432), the dogfood worker-drain gate (an unreadable cluster must never read
# as idle and let a teardown strand worker nodes, Q434), and the CI path-filter
# gate (a workspace module missing from a filter must fail, since the gate it
# would skip reports green either way, Q429). Lightweight pure-bash checks; part
# of `check` and the CI shellcheck job.
#
# The suites are independent and each isolates its own scratch state (mktemp -d,
# or a $$-suffixed dir under tmp/), so they run concurrently — labeled output via
# run-parallel.sh keeps a failure attributable to its suite.
SCRIPTS_TESTS := verify-release-test download-verified-test validate-cluster-test \
                 lint-backlog-test check-dep-advisory-test claude-go-throttle-hook-test \
                 dogfood/validate-release-test dogfood/pool-test dogfood/workers-test \
                 go-lint-scope-test \
                 check-roadmap-test check-conflict-markers-test check-v2-api-sync-test \
                 dependabot-rebase-stale-test go-vet-tags-test local-throttle-test \
                 shellcheck-scripts-test release-version-hook-test check-path-filters-test

.PHONY: scripts-test
scripts-test: ## Run scripts/ behavioural assertions (release identity regexp, validate-cluster helpers, STATUS.md lint rules, dep-advisory, go-throttle hook, dogfood gate run resolution, dogfood pool sizing, dogfood worker-drain gate, go-lint scoping, shellcheck file selection, conflict-marker gate, v2 API sync gate, roadmap/backlog coherence gate, Dependabot bump extraction, build-tag coverage guard, pinned-download integrity, heavy-build slot sizing, announce-bar version hook, CI path-filter coverage)
	scripts/run-parallel.sh $(foreach suite,$(SCRIPTS_TESTS),"$(notdir $(suite)):scripts/$(suite).sh")

# Install the tracked git hooks for this clone by pointing core.hooksPath at the
# in-repo .githooks/ directory. The path is relative, so it resolves correctly in
# the main checkout and every linked worktree. Run once after cloning (scripts/setup.sh
# does this for you). Bypass a single commit with `git commit --no-verify`.
.PHONY: hooks
hooks: ## Install the tracked git hooks (sets core.hooksPath to .githooks)
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath -> .githooks (fast gofmt + STATUS.md gate on commit)"

# Diagnose the local toolchain: report which required/e2e/extended CLI tools are
# missing or installed-but-not-on-PATH, with per-OS install and PATH-fix hints.
# Runnable without the vendored tool binaries, so it works on a fresh clone.
.PHONY: doctor
doctor: ## Check required CLI tools are installed and on PATH (install/PATH-fix hints for any missing)
	scripts/check-tools.sh

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests and DeepCopy methods
	$(MAKE) -C api generate
	$(MAKE) -C cmd/gmc generate
	$(MAKE) -C cmd/agc generate

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
	scripts/docs-preview.sh serve

.PHONY: docs-build
docs-build: ## Build the static docs/marketing site into site/ (isolated venv)
	scripts/docs-preview.sh build

# The heavy phases (test: one workspace-wide `go test`; lint: a per-module
# loop) live in scripts/go-test.sh and
# scripts/go-lint.sh, which apply the local auto-throttle themselves
# (scripts/local-throttle.sh: parallelism cap + low-priority QoS prefix on an
# interactive GUI dev shell; no-op on CI/headless — rationale in that script's
# header). V=1 (or VERBOSE=1) streams `go test` output live (-v) for debugging
# a slow or hanging test; make exports command-line variables to recipe
# environments, so `make test V=1` (and `make check V=1`) reach the script.
.PHONY: test
test: ## Run unit tests for all modules (V=1 streams output live for debugging a hang)
	scripts/go-test.sh

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
test-race: ## Run unit tests under the race detector (the CI unit gate; throttled locally, full speed on CI)
	scripts/go-test.sh --race

# --- Test-coverage measurement + no-regression ratchet ---------------------
# scripts/coverage.sh measures per-module unit-test coverage (the same per-module
# `go test` the workspace requires — never a repo-root `./...`), filters out
# generated/wiring code, and gates against the recorded floor in
# coverage-baseline.txt. Like `make test`, the script applies the local throttle
# prefix so a run on a GUI dev machine stays desktop-safe; on CI it is a no-op.
# We gate by a no-regression ratchet, not an absolute percentage — see
# docs/development/testing.md and docs/plan/release-1.0.md §F.
.PHONY: cover
cover: ## Report per-module unit-test coverage (writes nothing)
	scripts/coverage.sh report

.PHONY: cover-update
cover-update: ## Re-record the coverage baseline floor (coverage-baseline.txt)
	scripts/coverage.sh update

.PHONY: cover-check
cover-check: ## Fail if any module drops below its recorded coverage floor (the CI gate)
	scripts/coverage.sh check

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
	GOLANGCI_LINT=$(GOLANGCI_LINT) scripts/go-lint.sh

.PHONY: lint-backlog
lint-backlog: ## Enforce backlog format rules on docs/STATUS.md (vendored from the backlog skill)
	scripts/lint-backlog.sh

# IDs come from a ref claim, not from a counter line in STATUS.md: a shared
# mutable counter conflicted by construction whenever two sessions filed a row
# (Q382). N=<n> claims several at once; PEEK=1 shows the next without claiming.
.PHONY: queue-id
queue-id: ## Allocate the next backlog Q-ID (make queue-id [N=3] [PEEK=1])
	@scripts/alloc-queue-id.sh $(if $(PEEK),--peek,) $(if $(N),-n $(N),)

# The public roadmap and the backlog drift apart silently — a 2026-07-25 audit
# found six of seven "near-term" items already shipped. Because done Queue rows
# are deleted, a roadmap bullet naming a Q-ID that STATUS.md no longer has is an
# exact signal the work shipped. Rationale + the annotation format are in the
# script header.
.PHONY: roadmap-check
roadmap-check: ## Fail when docs/roadmap.md names backlog rows that shipped or moved tables
	scripts/check-roadmap.sh

# Catches the "closed plan never archived" drift that makes docs/plan/README.md
# read as stale. Rationale + the ⓘ exemption live in the script header.
.PHONY: plan-index-check
plan-index-check: ## Assert active plans in docs/plan/README.md are still STATUS-referenced (else archive them)
	scripts/check-plan-index.sh

# Keeps plan archival a docs-only operation: code that path-links a plan would
# force a code edit (and heavy CI) when that plan is archived. Rationale in the
# script header.
.PHONY: no-plan-refs-check
no-plan-refs-check: ## Assert Go code doesn't reference docs/plan/ paths (cite durable docs / Q-IDs instead)
	scripts/check-no-plan-refs-in-code.sh

# Without this gate, standalone helper scripts ship unlinted: actionlint only
# covers inline workflow `run:` blocks. Glob, version pin, and rationale live
# in the script header.
.PHONY: shellcheck
shellcheck: ## Shellcheck every present scripts/*.sh — tracked or untracked-and-not-gitignored (recursive; matches the CI shellcheck gate)
	scripts/shellcheck-scripts.sh

.PHONY: queue-unblock
queue-unblock: ## List Queue items blocked by ID=<id> (e.g. make queue-unblock ID=Q12; bare 12 also accepted)
	@if [ -z "$(ID)" ]; then echo "Usage: make queue-unblock ID=<id>" >&2; exit 1; fi
	@scripts/queue-unblock.sh $(ID)

# Consolidated third-party license attribution. scripts/gen-third-party-notices.sh
# concatenates every vendored module's LICENSE/NOTICE/COPYING text into the
# committed THIRD-PARTY-NOTICES file, which each production Dockerfile COPYs into
# /licenses/ to satisfy the reproduce-the-notice clauses of the bundled deps
# (Apache-2.0 §4(d), MIT/BSD). It reads only the committed, version-pinned
# vendor/ tree (offline, deterministic). Generate-and-commit so the content is
# reviewable in the diff; `-check` is the CI drift gate (license-notices.yml).
.PHONY: third-party-notices
third-party-notices: ## Regenerate THIRD-PARTY-NOTICES from the committed vendor/ tree
	scripts/gen-third-party-notices.sh

.PHONY: third-party-notices-check
third-party-notices-check: ## Fail if THIRD-PARTY-NOTICES is stale vs vendor/ (CI drift gate)
	scripts/gen-third-party-notices.sh --check

# Supply-chain integrity gate for the committed vendor trees. `-mod=vendor` only
# checks modules.txt consistency, never that the vendored source matches go.sum;
# this re-vendors (re-fetching modules verified against go.sum) and fails on any
# diff, so a tampered vendor/ edit can't ship into the signed release images
# (Q126). Runs the network re-fetch, so it stays out of the fast `make check`
# gate and runs as its own CI job (unit-test.yml vendor-check).
.PHONY: vendor-check
vendor-check: ## Fail if vendor/ + tools/vendor/ drift from go.sum (CI supply-chain gate)
	scripts/vendor-check.sh

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
	scripts/go-tidy-check.sh

# One-shot remedy for the three drift gates above. Runs the full "Changing
# dependencies" flow — tidy + go work sync + re-vendor (workspace + tools) +
# regenerate THIRD-PARTY-NOTICES — mutating the working tree so the committed
# go.mod/go.sum, vendor/, and THIRD-PARTY-NOTICES line back up. It is the fix a
# human runs after a dependency change, and what the dependabot-go-sync workflow
# runs to auto-repair a Dependabot Go bump (Q111), which can't run `go work
# vendor` itself. No-ops cleanly when nothing drifted.
.PHONY: vendor-sync
vendor-sync: ## Re-sync module files + vendor trees + THIRD-PARTY-NOTICES (the dependency-change / Dependabot remedy)
	scripts/vendor-sync.sh

##@ Security

# The security gates are scripted (scripts/{go-vulncheck,trivy-scan,
# polaris-scan,manifest-validate}.sh) and mirror their CI jobs exactly so local
# and CI verdicts match. Parameters, defaults, and rationale live in each
# script's header; all are env-overridable, and make exports command-line
# variables, so e.g. `make trivy-scan TRIVY_SEVERITY=CRITICAL` or
# `make manifest-validate MANIFEST_K8S_VERSION=1.31.0` reach the script.

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Run govulncheck across all workspace modules (matches the CI govulncheck gate)
	GOVULNCHECK=$(GOVULNCHECK) scripts/go-vulncheck.sh

.PHONY: trivy-scan
trivy-scan: ## Build each image locally and scan it with trivy (requires trivy + docker on PATH; matches the CI trivy gate)
	scripts/trivy-scan.sh

.PHONY: polaris-scan
polaris-scan: ## Render the Helm chart and audit its Kubernetes posture with polaris (gates on danger findings; requires helm + polaris on PATH; matches the CI polaris gate)
	scripts/polaris-scan.sh

.PHONY: chart-crds
chart-crds: ## Regenerate the Helm chart CRD templates from the controller-gen sources (single source of truth, Q73/Q142)
	scripts/sync-chart-crds.sh

.PHONY: chart-crds-check
chart-crds-check: ## Fail if the chart CRD templates drifted from their sources, or the GMC-bundled RunnerGroup CRD drifted from the AGC copy (Q73)
	scripts/sync-chart-crds.sh --check

.PHONY: chart-rbac
chart-rbac: ## Regenerate the Helm chart manager-role rules fragment from the controller-gen source (single source of truth, Q142)
	scripts/sync-chart-rbac.sh

.PHONY: chart-rbac-check
chart-rbac-check: ## Fail if the chart manager-role rules fragment drifted from cmd/gmc/config/rbac/role.yaml (Q142)
	scripts/sync-chart-rbac.sh --check

.PHONY: chart-webhook
chart-webhook: ## Regenerate the Helm chart validating-webhook template from the controller-gen source (single source of truth, Q143)
	scripts/sync-chart-webhook.sh

.PHONY: chart-webhook-check
chart-webhook-check: ## Fail if the chart webhook template drifted from cmd/gmc/config/webhook/manifests.yaml (Q143)
	scripts/sync-chart-webhook.sh --check

.PHONY: manifest-validate
manifest-validate: ## Validate the static install manifests + Helm chart (yamllint + kubeconform + helm lint; requires yamllint, kubeconform, helm on PATH; matches the CI manifest-validate gate)
	scripts/sync-chart-crds.sh --check
	scripts/sync-chart-rbac.sh --check
	scripts/sync-chart-webhook.sh --check
	scripts/manifest-validate.sh

##@ Operations

# Pre-install cluster preflight (Q184). Validates the target cluster can uphold
# tenant isolation BEFORE `helm install`: CNI NetworkPolicy enforcement (the
# critical one — kindnet silently voids it = hard fail), Kubernetes >= 1.30,
# cert-manager, and metrics-server. Detection-based (no workloads scheduled), so
# it is safe to run against a fresh cluster. KUBECTL/VALIDATE_STRICT env-override
# the binary and warning strictness (see the script header). Operators run this
# as the required first install step (docs/operations/install.md).
.PHONY: validate-cluster
validate-cluster: ## Preflight the target cluster before install (CNI enforcement, K8s>=1.30, cert-manager, metrics-server)
	scripts/validate-cluster.sh

##@ e2e

.PHONY: e2e-up
e2e-up: e2e-cluster e2e-images e2e ## One-shot: create cluster, build+push images, run all e2e suites

.PHONY: e2e-registry
e2e-registry: ## Start just the local OCI registry (no-op if already running)
	REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) \
		scripts/start-registry.sh

.PHONY: e2e-cluster
e2e-cluster: ## Create the local kind cluster + registry (no-op if both exist)
	KIND_CLUSTER=$(KIND_CLUSTER) KIND_CONFIG=$(KIND_CONFIG) \
		REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) \
		KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) \
		KIND_CNI=$(KIND_CNI) CALICO_VERSION=$(CALICO_VERSION) \
		scripts/kind-with-registry.sh

.PHONY: apply-cert-manager
apply-cert-manager: ## Apply cert-manager manifests (version defined in cmd/gmc/Makefile)
	$(MAKE) -C cmd/gmc apply-cert-manager

.PHONY: wait-cert-manager
wait-cert-manager: ## Wait for cert-manager deployments to be Available
	$(MAKE) -C cmd/gmc wait-cert-manager

.PHONY: install-cert-manager
install-cert-manager: ## Apply cert-manager and wait for it to be ready
	$(MAKE) -C cmd/gmc install-cert-manager

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
# SUITE=single-node|multi-node filters to a subset for local iteration; unset runs all specs.
# single-node maps to --label-filter '!multi-node' (tests that run on a 1-worker cluster).
SUITE ?=
_SUITE_FILTER = $(if $(filter single-node,$(SUITE)),!multi-node,$(if $(filter multi-node,$(SUITE)),multi-node,))

_GINKGO_RUN = cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
	GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) WORKER_IMG=$(WORKER_IMG) WRAPPER_IMG=$(WRAPPER_IMG) \
	$(GINKGO) run --tags e2e --timeout 30m --github-output --poll-progress-after 30s

# The JUnit report lives under the repo-local tmp/ (gitignored), not /tmp:
# host-wide temp is shared across worktrees/sessions (concurrent runs collide)
# and sits outside the workspace, where sandboxed tooling can't read it back
# when diagnosing a failed run.
.PHONY: e2e
e2e: $(GINKGO) ## Run e2e tests; SUITE=standard|multi-node selects a subset, unset runs all specs
	@mkdir -p $(REPO_ROOT)/tmp
	$(_GINKGO_RUN) $(if $(_SUITE_FILTER),--label-filter '$(_SUITE_FILTER)',) \
		--procs 6 --junit-report $(REPO_ROOT)/tmp/e2e-report.xml ./test/e2e/...

.PHONY: e2e-clean
e2e-clean: e2e-cluster-delete e2e-registry-delete ## Tear down the e2e cluster and registry, and delete .build/
	rm -rf .build

##@ Tools

.PHONY: tools
tools: $(CONTROLLER_GEN) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GINKGO) $(GOLANGCI_LINT) $(GOVULNCHECK) ## Build all vendored build tools into .build/

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Build golangci-lint into .build/

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Build govulncheck into .build/

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST) ## Build setup-envtest into .build/

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Build ginkgo into .build/

.PHONY: cosign
cosign: $(COSIGN) ## Download pinned cosign (COSIGN_VERSION) into .build/

.PHONY: verify-release
verify-release: $(COSIGN) ## Verify cosign signatures for a published release: make verify-release VERSION=vX.Y.Z
	@COSIGN=$(COSIGN) scripts/verify-release.sh $(VERSION)

# The kubebuilder-ecosystem tools all build the same way from the vendored
# tools/ module; only the package path differs (the target-specific TOOL_PKG
# below). ginkgo is the exception: it builds from cmd/gmc (workspace on) so it
# matches the ginkgo version the e2e suite imports.
$(CONTROLLER_GEN): TOOL_PKG := sigs.k8s.io/controller-tools/cmd/controller-gen
$(KUBEBUILDER):    TOOL_PKG := sigs.k8s.io/kubebuilder/v4
$(SETUP_ENVTEST):  TOOL_PKG := sigs.k8s.io/controller-runtime/tools/setup-envtest
$(GOLANGCI_LINT):  TOOL_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
$(GOVULNCHECK):    TOOL_PKG := golang.org/x/vuln/cmd/govulncheck

$(CONTROLLER_GEN) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GOLANGCI_LINT) $(GOVULNCHECK):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ $(TOOL_PKG)

$(GINKGO):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/cmd/gmc && go build -o $@ github.com/onsi/ginkgo/v2/ginkgo

# cosign is a non-Go-vendored binary tool (its dependency tree is too large to
# vendor like the kubebuilder-ecosystem tools above), so it is downloaded at a
# pinned version — the same pattern as the shellcheck/kubeconform CI installs.
$(COSIGN):
	scripts/download-cosign.sh $@ $(COSIGN_VERSION)
