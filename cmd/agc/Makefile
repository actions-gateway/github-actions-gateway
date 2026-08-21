# Makefile for the Actions Gateway Controller (AGC).
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT      := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
LOCALBIN       := $(REPO_ROOT)/.build
YEAR           ?= $(shell date +%Y)

ENVTEST_K8S_VERSION ?= 1.35.x

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: all
all: generate build test ## Generate manifests, build the binary, run tests

.PHONY: build
build: ## Build the agc binary into .build/
	go build -o ../../.build/agc .

.PHONY: test
test: ## Run unit tests (no envtest required)
	go test -race -timeout 2m ./...

# RUN narrows the run to matching test names (Q592). The two halves a
# hand-assembled KUBEBUILDER_ASSETS gets wrong are already right here:
# ENVTEST_K8S_VERSION is the pin, and --bin-dir is absolute, since `-p path`
# echoes the shape it is given and each test binary resolves a relative one
# against its own package dir. -v -count=1 ride along with RUN: a targeted run
# wants the test's output, and a cached PASS prints none.
# go-test-integration.sh owns -race, the `integration` tag and the -timeout
# budget, so the budget is one number for both modules and both CI steps, and a
# breach of it says the suite ran out of time rather than blaming whichever test
# held the wall (Q741). It owns the RUN contract too: the -v -count=1 a filtered
# run needs, and the failure when the filter selects nothing (Q736).
.PHONY: test-integration
test-integration: manifests $(SETUP_ENVTEST) ## Run envtest-backed integration tests; RUN='TestName' narrows to matching tests
	KUBEBUILDER_ASSETS="$(shell "$(SETUP_ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		$(REPO_ROOT)/scripts/go/go-test-integration.sh cmd/agc $(if $(RUN),-run '$(RUN)') ./...

# The live-autoscaler drift gate (Q474). Runs against the kind cluster
# `make autoscaler-cluster` stands up, and fails rather than skips when that
# cluster is absent — a drift detector that skips itself detects nothing.
#
# No -race: this test drives a cluster and asserts on its replies; the code under
# test is one pure function, and the concurrency core it would instrument is not
# on this path. -count=1 because the answer is the cluster's, not the build
# cache's. The timeout covers three cases at up to 3m of autoscaler-loop waiting.
.PHONY: test-autoscaler
test-autoscaler: ## Assert the autoscaler matcher against a live cluster-autoscaler (needs `make autoscaler-cluster`)
	go test -v -count=1 -timeout 15m -tags autoscaler -run 'TestLiveClusterAutoscaler' ./internal/controller/...

.PHONY: test-karpenter
test-karpenter: ## Assert the autoscaler matcher against a live Karpenter (needs `make karpenter-cluster`)
	go test -v -count=1 -timeout 15m -tags karpenter -run 'TestLiveKarpenter' ./internal/controller/...

##@ Load test

# The in-process AGC load harness (Q13, cmd/agc/test/load). It needs no cluster
# and no real credentials — it drives the real listener-multiplexing core
# against an in-process broker stub + fake k8s client. LOAD_THROTTLE is the
# desktop-safety QoS prefix from scripts/agent/local-throttle.sh (a no-op on
# CI/headless): a run holds ~1,000 goroutines, so on a GUI dev machine it is
# demoted to keep the UI responsive (Q92). Tune any run via the LOAD_* env vars
# documented in test/load/load_test.go. NOT run under -race by default: -race on
# ~1,000 goroutines is heavy and distorts the throughput/memory numbers; add it
# for a smaller leak hunt (LOAD_LISTENERS_PER_TENANT=10 ... go test -race).
LOAD_THROTTLE := $(shell $(REPO_ROOT)/scripts/agent/local-throttle.sh prefix)

.PHONY: load-test-quick
load-test-quick: ## Load smoke: 10 tenants × 100 sessions = 1,000, short window (~1 min)
	LOAD_DURATION=$(or $(LOAD_DURATION),20s) LOAD_JOB_DURATION=$(or $(LOAD_JOB_DURATION),100ms) \
		$(LOAD_THROTTLE) go test -tags load -timeout 5m -run TestAGCLoad -v ./test/load/...

.PHONY: load-test-full
load-test-full: ## Load acceptance: 10 tenants × 100 sessions = 1,000, realistic hold (~3-5 min)
	LOAD_DURATION=$(or $(LOAD_DURATION),60s) LOAD_JOB_DURATION=$(or $(LOAD_JOB_DURATION),1s) \
	LOAD_REPORT=$(or $(LOAD_REPORT),results/latest.md) \
		$(LOAD_THROTTLE) go test -tags load -timeout 10m -run TestAGCLoad -v ./test/load/...

.PHONY: mem-profile
mem-profile: ## Isolate AGC-only per-session memory (Q181): 1,000 parked sessions, in-process transport, no broker stub
	MEM_TENANTS=$(or $(MEM_TENANTS),10) MEM_LISTENERS_PER_TENANT=$(or $(MEM_LISTENERS_PER_TENANT),100) \
		$(LOAD_THROTTLE) go test -tags load -timeout 5m -run TestAGCPerSessionMemory -v ./test/load/...

.PHONY: generate
generate: manifests deepcopy ## Regenerate CRD/RBAC manifests and DeepCopy methods

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) crd rbac:roleName=agc-role paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

.PHONY: deepcopy
deepcopy: $(CONTROLLER_GEN) ## Regenerate DeepCopy methods from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(SETUP_ENVTEST):
	$(MAKE) -C $(REPO_ROOT) setup-envtest
