# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
GINKGO         := $(REPO_ROOT)/.build/ginkgo

KIND_CLUSTER  ?= actions-gateway-e2e
# KIND_CONFIG defaults to the 2-node config used by the standard (non-multi-node)
# e2e suite. The multi-node suite needs 3 nodes — pass
# KIND_CONFIG=test/kind-config.yaml when creating the cluster for `e2e-multi-node`
# or `e2e-all`.
KIND_CONFIG   ?= test/kind-config-ci.yaml
GIT_SHA       := $(shell git rev-parse --short HEAD)
GMC_IMG       ?= gmc:e2e-$(GIT_SHA)
AGC_IMG       ?= agc:e2e
PROXY_IMG     ?= proxy:e2e
FAKEGITHUB_IMG ?= fakegithub:e2e

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: all
all: build ## Alias for `build`

.PHONY: build
build: build-agc build-probe ## Build the agc and probe binaries into .build/

.PHONY: build-agc
build-agc: ## Build the AGC binary
	go build -C cmd/agc -o ../../.build/agc .

.PHONY: build-probe
build-probe: ## Build the probe binary
	go build -C cmd/probe -o ../../.build/probe .

##@ e2e

.PHONY: e2e-up
e2e-up: e2e-cluster e2e-load-images e2e ## One-shot: create cluster, build+load images, run the standard e2e suite

.PHONY: e2e-cluster
e2e-cluster: ## Create the local e2e kind cluster (no-op if it already exists)
	@if kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then \
		echo "==> kind cluster $(KIND_CLUSTER) already exists"; \
	else \
		echo "==> creating kind cluster $(KIND_CLUSTER) ($(KIND_CONFIG))"; \
		kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG); \
	fi

.PHONY: e2e-cluster-delete
e2e-cluster-delete: ## Delete the local e2e kind cluster (no-op if it does not exist)
	@if kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then \
		echo "==> deleting kind cluster $(KIND_CLUSTER)"; \
		kind delete cluster --name $(KIND_CLUSTER); \
	else \
		echo "==> kind cluster $(KIND_CLUSTER) does not exist"; \
	fi

.PHONY: e2e-images
e2e-images: docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub ## Build all four e2e Docker images

.PHONY: docker-build-gmc
docker-build-gmc: ## Build the GMC Docker image
	docker build -f cmd/gmc/Dockerfile -t $(GMC_IMG) .

.PHONY: docker-build-agc
docker-build-agc: ## Build the AGC Docker image
	docker build -f cmd/agc/Dockerfile -t $(AGC_IMG) .

.PHONY: docker-build-proxy
docker-build-proxy: ## Build the egress proxy Docker image
	docker build -f cmd/proxy/Dockerfile -t $(PROXY_IMG) cmd/proxy

.PHONY: docker-build-fakegithub
docker-build-fakegithub: ## Build the fakegithub test-fixture Docker image
	docker build -f test/fakegithub/Dockerfile -t $(FAKEGITHUB_IMG) .

.PHONY: e2e-load-images
e2e-load-images: e2e-images ## Build and load the four e2e images into the kind cluster
	kind load docker-image $(GMC_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(AGC_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(PROXY_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(FAKEGITHUB_IMG) --name $(KIND_CLUSTER)

# Run Tier A + Tier B e2e tests (excludes multi-node tests).
# Uses the ginkgo CLI so --procs and --label-filter are recognised.
.PHONY: e2e
e2e: $(GINKGO) ## Run the standard e2e suite (Tier A + Tier B; excludes multi-node)
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		$(GINKGO) run \
		--tags e2e --timeout 30m \
		--label-filter '!multi-node' --procs 8 \
		--github-output --poll-progress-after 60s \
		--junit-report /tmp/e2e-report.xml \
		./test/e2e/...

# Run multi-node e2e tests (requires 3-node cluster; see test/kind-config.yaml).
# Uses --procs=3 so the three suites' BeforeAll deployment waits overlap.
.PHONY: e2e-multi-node
e2e-multi-node: $(GINKGO) ## Run the multi-node e2e suite (HPA load, PDB drain — requires 3-node cluster)
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		$(GINKGO) run \
		--tags e2e --timeout 30m \
		--label-filter 'multi-node' --procs 3 \
		--github-output --poll-progress-after 60s \
		--junit-report /tmp/e2e-local-report.xml \
		./test/e2e/...

.PHONY: e2e-all
e2e-all: $(GINKGO) ## Run every e2e suite, including multi-node (requires 3-node cluster)
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		$(GINKGO) run \
		--tags e2e --timeout 30m \
		--procs 8 \
		./test/e2e/...

.PHONY: e2e-clean
e2e-clean: e2e-cluster-delete ## Tear down the e2e kind cluster

##@ Tools

.PHONY: tools
tools: $(CONTROLLER_GEN) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GINKGO) ## Build all vendored build tools into .build/

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
