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
# KIND_CONFIG defaults to the 2-worker config so all test suites work out of the box.
# Override with test/kind-config-1worker.yaml if you only need the standard suite and want a faster cluster.
KIND_CONFIG   ?= test/kind-config-2worker.yaml
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

.DEFAULT_GOAL := help

.PHONY: all build build-agc build-gmc build-probe build-proxy tools setup-envtest \
        e2e-cluster e2e-cluster-delete e2e-images e2e e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub \
        ginkgo

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

##@ e2e

.PHONY: e2e-up
e2e-up: e2e-cluster e2e-images e2e ## One-shot: create cluster, build+push images, run all e2e suites

.PHONY: e2e-cluster
e2e-cluster: ## Create the local kind cluster + registry (no-op if both exist)
	KIND_CLUSTER=$(KIND_CLUSTER) KIND_CONFIG=$(KIND_CONFIG) \
		REGISTRY_NAME=$(REGISTRY_NAME) REGISTRY_PORT=$(REGISTRY_PORT) \
		scripts/kind-with-registry.sh

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
# SUITE=standard|multi-node filters to a subset for local iteration; unset runs all specs.
SUITE ?=
_SUITE_FILTER = $(if $(filter standard,$(SUITE)),!multi-node,$(if $(filter multi-node,$(SUITE)),multi-node,))

_GINKGO_RUN = cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
	GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
	$(GINKGO) run --tags e2e --timeout 30m --github-output --poll-progress-after 60s

.PHONY: e2e
e2e: $(GINKGO) ## Run e2e tests; SUITE=standard|multi-node selects a subset, unset runs all specs
	$(_GINKGO_RUN) $(if $(_SUITE_FILTER),--label-filter '$(_SUITE_FILTER)',) \
		--procs 4 --junit-report /tmp/e2e-report.xml ./test/e2e/...

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
