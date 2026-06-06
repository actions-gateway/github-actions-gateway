# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
GINKGO         := $(REPO_ROOT)/.build/ginkgo
GOLANGCI_LINT  := $(REPO_ROOT)/.build/golangci-lint
GOVULNCHECK    := $(REPO_ROOT)/.build/govulncheck

KIND_CLUSTER  ?= actions-gateway-e2e
# KIND_CONFIG defaults to the 2-worker config so all test suites work out of the box.
# Override with test/kind-config-1worker.yaml if you only need the standard suite and want a faster cluster.
KIND_CONFIG   ?= test/kind-config-2worker.yaml
# KIND_NODE_IMAGE pins the node image (and thus the cluster's K8s version) when set.
# Left empty here so local runs use the installed kind's default; CI sets it to a
# digest-pinned kindest/node so the image can be cached and reused across runs.
KIND_NODE_IMAGE ?=
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

.PHONY: all check hooks generate build build-agc build-gmc build-probe build-proxy test test-integration tools setup-envtest \
        e2e-registry e2e-cluster e2e-cluster-delete e2e-images e2e e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub \
        ginkgo golangci-lint lint lint-status queue-unblock \
        vulncheck govulncheck trivy-scan

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
# PR: it runs exactly what the unit-test CI workflow runs (gofmt + golangci-lint,
# STATUS.md format lint, unit tests), so a green `make check` means a green
# unit-test workflow. The slower security gates (vulncheck, trivy-scan) and the
# integration/e2e tiers stay separate so this loop stays fast.
.PHONY: check
check: lint lint-status test ## Fast pre-review gate: gofmt + golangci-lint + STATUS.md lint + unit tests (mirrors the unit-test CI workflow)

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

.PHONY: test
test: ## Run unit tests for all modules
	cd broker     && go test ./...
	cd githubapp  && go test ./...
	cd cmd/agc   && go test ./...
	cd cmd/gmc   && go test ./...
	cd cmd/probe && go test ./...
	cd cmd/proxy && go test ./...
	cd cmd/worker && go test ./...

.PHONY: test-integration
test-integration: ## Run envtest-backed integration tests for cmd/agc and cmd/gmc
	$(MAKE) -C cmd/agc test-integration
	$(MAKE) -C cmd/gmc test-integration

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run gofmt and golangci-lint across all workspace modules (golangci-lint includes govet)
	@set -euo pipefail; \
	unformatted=$$(gofmt -l $$(go work edit -json | jq -r '.Use[].DiskPath')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files are not formatted:"; \
		echo "$$unformatted"; \
		echo "run: gofmt -w <file>"; \
		exit 1; \
	fi
	@set -euo pipefail; \
	for dir in $$(go work edit -json | jq -r '.Use[].DiskPath'); do \
		echo "==> golangci-lint $$dir"; \
		(cd "$$dir" && $(GOLANGCI_LINT) run --config $(REPO_ROOT)/.golangci.yml ./...) || exit 1; \
	done

.PHONY: lint-status
lint-status: ## Enforce churn-reduction format rules on docs/STATUS.md
	scripts/lint-status.sh

.PHONY: queue-unblock
queue-unblock: ## List Queue items blocked by ID=<id> (e.g. make queue-unblock ID=Q12; bare 12 also accepted)
	@if [ -z "$(ID)" ]; then echo "Usage: make queue-unblock ID=<id>" >&2; exit 1; fi
	@scripts/queue-unblock.sh $(ID)

##@ Security

# trivy image scan parameters, mirrored exactly by the CI `trivy` job so local
# and CI verdicts match. ignore-unfixed drops CVEs with no released fix (nothing
# actionable here); only fixable HIGH/CRITICAL findings fail the scan.
TRIVY_SEVERITY ?= HIGH,CRITICAL
# Each entry is "<name>=<dockerfile>" — the five images the CI trivy matrix scans.
TRIVY_IMAGES   := gmc=cmd/gmc/Dockerfile agc=cmd/agc/Dockerfile proxy=cmd/proxy/Dockerfile worker=cmd/worker/Dockerfile fakegithub=test/fakegithub/Dockerfile
# Images scanned report-only (findings printed, never fail the scan): the worker
# image is built FROM the upstream actions-runner and carries upstream CVEs we
# cannot fix. Matches the worker leg's exit-code 0 in security-scan.yml.
TRIVY_REPORT_ONLY := worker

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Run govulncheck across all workspace modules (matches the CI govulncheck gate)
	@set -euo pipefail; \
	for dir in $$(go work edit -json | jq -r '.Use[].DiskPath'); do \
		echo "==> govulncheck $$dir"; \
		(cd "$$dir" && $(GOVULNCHECK) ./...) || exit 1; \
	done

.PHONY: trivy-scan
trivy-scan: ## Build each image locally and scan it with trivy (requires trivy + docker on PATH; matches the CI trivy gate)
	@command -v trivy >/dev/null 2>&1 || { echo "trivy not found on PATH — install: https://trivy.dev/latest/getting-started/installation/" >&2; exit 1; }
	@set -euo pipefail; \
	for entry in $(TRIVY_IMAGES); do \
		name="$${entry%%=*}"; dockerfile="$${entry#*=}"; \
		code=1; for ro in $(TRIVY_REPORT_ONLY); do [[ "$$ro" == "$$name" ]] && code=0; done; \
		echo "==> building local/$$name:trivy from $$dockerfile"; \
		docker buildx build --load -t "local/$$name:trivy" -f "$$dockerfile" .; \
		echo "==> trivy image local/$$name:trivy (exit-code $$code)"; \
		trivy image --severity "$(TRIVY_SEVERITY)" --ignore-unfixed --exit-code "$$code" "local/$$name:trivy" || exit 1; \
	done

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
# load step — kind nodes pull from there on demand.
.PHONY: e2e-images
e2e-images: ## Build and push all four e2e images in parallel via docker buildx bake
	GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) \
		docker buildx bake --file docker-bake.hcl

.PHONY: docker-build-gmc
docker-build-gmc: ## Build and push only the GMC image (bake target `gmc`)
	GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) \
		docker buildx bake --file docker-bake.hcl gmc

.PHONY: docker-build-agc
docker-build-agc: ## Build and push only the AGC image (bake target `agc`)
	GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) \
		docker buildx bake --file docker-bake.hcl agc

.PHONY: docker-build-proxy
docker-build-proxy: ## Build and push only the egress proxy image (bake target `proxy`)
	GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) \
		docker buildx bake --file docker-bake.hcl proxy

.PHONY: docker-build-fakegithub
docker-build-fakegithub: ## Build and push only the fakegithub image (bake target `fakegithub`)
	GIT_SHA=$(GIT_SHA) IMAGE_REGISTRY=$(IMAGE_REGISTRY) \
		docker buildx bake --file docker-bake.hcl fakegithub

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

$(CONTROLLER_GEN):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-tools/cmd/controller-gen

$(KUBEBUILDER):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/kubebuilder/v4

$(SETUP_ENVTEST):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-runtime/tools/setup-envtest

$(GINKGO):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/cmd/gmc && go build -o $@ github.com/onsi/ginkgo/v2/ginkgo

$(GOLANGCI_LINT):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ github.com/golangci/golangci-lint/v2/cmd/golangci-lint

$(GOVULNCHECK):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ golang.org/x/vuln/cmd/govulncheck
