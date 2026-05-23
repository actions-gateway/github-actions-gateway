# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest

KIND_CLUSTER  ?= actions-gateway-e2e
# KIND_CONFIG defaults to the 3-node local config.
# CI uses KIND_CONFIG=test/kind-config-ci.yaml (2-node, no local-only tests).
KIND_CONFIG   ?= test/kind-config.yaml
GIT_SHA       := $(shell git rev-parse --short HEAD)
GMC_IMG       ?= gmc:e2e-$(GIT_SHA)
AGC_IMG       ?= agc:e2e
PROXY_IMG     ?= proxy:e2e
FAKEGITHUB_IMG ?= fakegithub:e2e

.PHONY: all build build-agc build-probe tools setup-envtest \
        e2e-cluster e2e-cluster-delete e2e-images e2e e2e-local-only e2e-all e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub

all: build

build: build-agc build-probe

build-agc:
	go build -C cmd/agc -o ../../.build/agc .

build-probe:
	go build -C cmd/probe -o ../../.build/probe .

# ── e2e cluster management ─────────────────────────────────────────────────────

# Create a multi-node kind cluster for e2e tests.
e2e-cluster:
	kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG) || true

# Delete the e2e kind cluster.
e2e-cluster-delete:
	kind delete cluster --name $(KIND_CLUSTER) || true

# Build all four Docker images required for e2e tests.
e2e-images: docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub

docker-build-gmc:
	docker build -f cmd/gmc/Dockerfile -t $(GMC_IMG) .

docker-build-agc:
	docker build -f cmd/agc/Dockerfile -t $(AGC_IMG) .

docker-build-proxy:
	docker build -f cmd/proxy/Dockerfile -t $(PROXY_IMG) cmd/proxy

docker-build-fakegithub:
	docker build -f test/fakegithub/Dockerfile -t $(FAKEGITHUB_IMG) .

# Load images into the kind cluster.
e2e-load-images:
	kind load docker-image $(GMC_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(AGC_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(PROXY_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(FAKEGITHUB_IMG) --name $(KIND_CLUSTER)

# Run Tier A + Tier B e2e tests (excludes local-only tests).
# Note: Ginkgo v2 flags must use -args with -ginkgo.<flag>=<value> syntax when
# invoked via go test. The -- separator passes raw args to the test binary but
# --label-filter is not recognized; only -ginkgo.label-filter= works.
e2e:
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		go test -v -tags e2e -count=1 -timeout 30m ./test/e2e/... \
		-args -ginkgo.label-filter='!local-only' -ginkgo.procs=8 \
		      -ginkgo.github-output -ginkgo.poll-progress-after=60s \
		      -ginkgo.junit-report=/tmp/e2e-report.xml

# Run local-only e2e tests (requires 3-node cluster; see test/kind-config.yaml).
# Uses --procs=3 so the three suites' BeforeAll deployment waits overlap.
e2e-local-only:
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		go test -v -tags e2e -count=1 -timeout 30m ./test/e2e/... \
		-args -ginkgo.label-filter='local-only' -ginkgo.procs=3 \
		      -ginkgo.github-output -ginkgo.poll-progress-after=60s \
		      -ginkgo.junit-report=/tmp/e2e-local-report.xml

# Run all e2e tests including local-only (HPA load, PDB drain).
e2e-all:
	cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
		GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
		go test -v -tags e2e -count=1 -timeout 30m ./test/e2e/...

# Tear down the e2e cluster.
e2e-clean: e2e-cluster-delete

# ── tools ──────────────────────────────────────────────────────────────────────

tools: $(CONTROLLER_GEN) $(KUBEBUILDER) $(SETUP_ENVTEST)

$(CONTROLLER_GEN):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-tools/cmd/controller-gen

$(KUBEBUILDER):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/kubebuilder/v4

setup-envtest: $(SETUP_ENVTEST)

$(SETUP_ENVTEST):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-runtime/tools/setup-envtest
