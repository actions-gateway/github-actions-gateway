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
IMAGE_REGISTRY ?= localhost:$(REGISTRY_PORT)
GMC_IMG        ?= $(IMAGE_REGISTRY)/gmc:e2e-$(GIT_SHA)
AGC_IMG        ?= $(IMAGE_REGISTRY)/agc:e2e-$(GIT_SHA)
PROXY_IMG      ?= $(IMAGE_REGISTRY)/proxy:e2e-$(GIT_SHA)
FAKEGITHUB_IMG ?= $(IMAGE_REGISTRY)/fakegithub:e2e-$(GIT_SHA)
WORKER_IMG     ?= $(IMAGE_REGISTRY)/worker:e2e-$(GIT_SHA)

.DEFAULT_GOAL := help

.PHONY: all check hooks generate build build-agc build-gmc build-probe build-proxy test test-race test-integration \
        cover cover-update cover-check tools setup-envtest \
        e2e-registry e2e-cluster e2e-cluster-delete e2e-images e2e e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub \
        ginkgo golangci-lint lint lint-status shellcheck queue-unblock \
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
.PHONY: check
check: lint lint-status go-version-check shellcheck chart-crds-check chart-rbac-check scripts-test test ## Fast pre-review gate: gofmt + golangci-lint + STATUS.md lint + single-Go-version + shellcheck + chart-CRD/RBAC drift + scripts-test + unit tests (CI also runs them under -race; see `make test-race`)

# Enforce the "all go modules use the same Go version" rule (Q68). The two
# go.work.gen files feed `make manifests` via GOWORK= and have silently drifted
# off the repo `go` directive before, breaking code generation. This asserts the
# `go` directive matches across go.work, every go.mod, and every go.work.gen.
.PHONY: go-version-check
go-version-check: ## Assert a single `go` directive across go.work / go.mod / go.work.gen
	scripts/check-go-version.sh

# Behavioural assertions for the scripts/ tree that shellcheck (a linter) can't
# express — currently the tags-only release signing-identity regexp (Q124).
# Lightweight pure-bash checks; part of `check` and the CI shellcheck job.
.PHONY: scripts-test
scripts-test: ## Run scripts/ behavioural assertions (e.g. release identity regexp)
	scripts/verify-release-test.sh

# Install the tracked git hooks for this clone by pointing core.hooksPath at the
# in-repo .githooks/ directory. The path is relative, so it resolves correctly in
# the main checkout and every linked worktree. Run once after cloning (scripts/setup.sh
# does this for you). Bypass a single commit with `git commit --no-verify`.
.PHONY: hooks
hooks: ## Install the tracked git hooks (sets core.hooksPath to .githooks)
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath -> .githooks (fast gofmt + STATUS.md gate on commit)"

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests and DeepCopy methods
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

.PHONY: build-probe
build-probe: ## Build the probe binary
	go build -C cmd/probe -o ../../.build/probe .

.PHONY: build-proxy
build-proxy: ## Build the proxy binary
	go build -C cmd/proxy -o ../../.build/proxy .

# The heavy per-module loops (test, lint) live in scripts/go-test.sh and
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

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run gofmt and golangci-lint across all workspace modules (golangci-lint includes govet)
	GOLANGCI_LINT=$(GOLANGCI_LINT) scripts/go-lint.sh

.PHONY: lint-status
lint-status: ## Enforce churn-reduction format rules on docs/STATUS.md
	scripts/lint-status.sh

# Without this gate, standalone helper scripts ship unlinted: actionlint only
# covers inline workflow `run:` blocks. Glob, version pin, and rationale live
# in the script header.
.PHONY: shellcheck
shellcheck: ## Shellcheck all tracked scripts/*.sh (recursive; matches the CI shellcheck gate)
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

.PHONY: manifest-validate
manifest-validate: ## Validate the static install manifests + Helm chart (yamllint + kubeconform + helm lint; requires yamllint, kubeconform, kustomize, helm on PATH; matches the CI manifest-validate gate)
	scripts/sync-chart-crds.sh --check
	scripts/sync-chart-rbac.sh --check
	scripts/manifest-validate.sh

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
	GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) WORKER_IMG=$(WORKER_IMG) \
	$(GINKGO) run --tags e2e --timeout 30m --github-output --poll-progress-after 30s

.PHONY: e2e
e2e: $(GINKGO) ## Run e2e tests; SUITE=standard|multi-node selects a subset, unset runs all specs
	$(_GINKGO_RUN) $(if $(_SUITE_FILTER),--label-filter '$(_SUITE_FILTER)',) \
		--procs 6 --junit-report /tmp/e2e-report.xml ./test/e2e/...

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
