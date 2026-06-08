# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.
# Run `make` (or `make help`) for the list of available targets.

# Recipes use bash-only constructs (`set -o pipefail`, `[[ ]]`). GNU make spawns
# /bin/sh for recipe lines, which is dash on the CI ubuntu runner and rejects
# `set -o pipefail`; pin the recipe shell to bash so recipes behave the same on
# CI and on dev machines (where /bin/sh already happens to be bash).
SHELL := /bin/bash

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

.PHONY: all check hooks generate build build-agc build-gmc build-probe build-proxy test test-race test-integration tools setup-envtest \
        e2e-registry e2e-cluster e2e-cluster-delete e2e-images e2e e2e-clean \
        docker-build-gmc docker-build-agc docker-build-proxy docker-build-fakegithub \
        ginkgo golangci-lint lint lint-status queue-unblock \
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
# PR: gofmt + golangci-lint, STATUS.md format lint, and the (plain) unit tests —
# the fast local loop. The CI `unit-test` job runs the same unit tests but under
# the race detector (`make test-race`); that heavier run stays out of `check` so
# the dev gate doesn't become an unthrottled `-race` run. A green `make check`
# covers the lint and unit-test logic; reproduce the race gate with `make
# test-race` when a change touches concurrency. The slower security gates
# (vulncheck, trivy-scan) and the integration/e2e tiers stay separate too.
.PHONY: check
check: lint lint-status test ## Fast pre-review gate: gofmt + golangci-lint + STATUS.md lint + unit tests (CI also runs them under -race; see `make test-race`)

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

# V=1 (or VERBOSE=1) streams test output live (-v). Off by default so the green
# path stays compressed — go test already prints one `ok pkg` line per passing
# package and the full output of any package that fails. Turn it on when
# debugging a slow or hanging test: without -v, go test buffers each package's
# output until the package completes, so a hung test shows nothing (not even its
# t.Log lines) until it finishes or hits -timeout; with -v the output streams as
# it is produced. Propagates through `make check` too (e.g. `make check V=1`).
GOTEST_V := $(if $(or $(V),$(VERBOSE)),-v,)

# --- Local resource auto-throttle (interactive GUI dev shell) --------------
# scripts/local-throttle.sh detects an interactive, GUI-bearing dev shell and
# emits a parallelism cap (physical cores − 2) plus a low-priority QoS command
# prefix (macOS: taskpolicy -c utility; Linux/WSL: nice -n 19 [+ ionice -c 3]).
# Without it a full `make check` saturates every core and makes the desktop
# unresponsive — on macOS the WindowServer watchdog then restarts the
# compositor and the GUI freezes. On CI (the CI env var is set), on headless or
# SSH Linux shells (no DISPLAY/WAYLAND_DISPLAY), and on unsupported OSes the
# script prints nothing, so all of these expand empty and the gate runs at full
# speed. Detail and rationale live in the script header.
THROTTLE_JOBS   := $(shell "$(REPO_ROOT)/scripts/local-throttle.sh" jobs)
THROTTLE_PREFIX := $(shell "$(REPO_ROOT)/scripts/local-throttle.sh" prefix)
# Per-process env + flags derived from the cap; empty (no-op) when not throttling.
# go test reads GOMAXPROCS/-p; golangci-lint ignores both and needs -j.
THROTTLE_ENV    := $(if $(THROTTLE_JOBS),GOMAXPROCS=$(THROTTLE_JOBS),)
GOTEST_P        := $(if $(THROTTLE_JOBS),-p $(THROTTLE_JOBS),)
GOLANGCI_J      := $(if $(THROTTLE_JOBS),-j $(THROTTLE_JOBS),)

.PHONY: test
test: ## Run unit tests for all modules (V=1 streams output live for debugging a hang)
	@set -euo pipefail; \
	for dir in $$(go work edit -json | jq -r '.Use[].DiskPath'); do \
		echo "==> go test $$dir"; \
		(cd "$$dir" && $(THROTTLE_ENV) $(THROTTLE_PREFIX) go test -timeout 2m $(GOTEST_P) $(GOTEST_V) ./...) || exit 1; \
	done

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
	@set -euo pipefail; \
	for dir in $$(go work edit -json | jq -r '.Use[].DiskPath'); do \
		echo "==> go test -race $$dir"; \
		(cd "$$dir" && $(THROTTLE_ENV) $(THROTTLE_PREFIX) go test -race -timeout 5m $(GOTEST_P) $(GOTEST_V) ./...) || exit 1; \
	done

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
		(cd "$$dir" && $(THROTTLE_ENV) $(THROTTLE_PREFIX) $(GOLANGCI_LINT) run $(GOLANGCI_J) --config $(REPO_ROOT)/.golangci.yml ./...) || exit 1; \
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

# polaris posture-scan parameters, mirrored exactly by the CI `polaris` job.
# POLARIS_RENDER_DIGEST is a placeholder sha256 digest used only to render the
# chart for the scan: production installs pin gmc.image.digest, so auditing the
# digest-pinned form reflects the SHIPPED posture. The un-pinned :latest default
# still trips the gating `tagNotSpecified` danger check, so this does not mask a
# real finding — it just lets the scan see the rest of the manifest. The value
# must satisfy values.schema.json's sha256:[a-f0-9]{64} pattern.
POLARIS_RENDER_DIGEST ?= sha256:1111111111111111111111111111111111111111111111111111111111111111
POLARIS_CHART         := $(REPO_ROOT)/charts/actions-gateway
POLARIS_CONFIG        := $(POLARIS_CHART)/polaris.yaml
POLARIS_RENDER        := $(REPO_ROOT)/.build/polaris-render.yaml

# Install-artifact validation parameters, mirrored exactly by the CI
# `manifest-validate` job (Q66). yamllint catches malformed/ill-formatted YAML;
# kubeconform schema-validates the rendered manifests against the cluster API.
# MANIFEST_K8S_VERSION is the chart's kubeVersion floor (Chart.yaml: >=1.30.0):
# validating against the oldest supported version catches a field that does not
# exist there. -ignore-missing-schemas skips resources whose schema is not in
# the upstream Kubernetes set — cert-manager (Certificate/Issuer), the
# Prometheus Operator (ServiceMonitor) and our own CRs (ActionsGateway/
# RunnerGroup). Those are third-party/custom kinds; the CRDs that define them
# ARE validated (CustomResourceDefinition is a native apiextensions kind).
MANIFEST_K8S_VERSION ?= 1.30.0
# kubeconform fetches missing schemas over the network. Set KUBECONFORM_CACHE to
# a directory to persist them between runs (CI points it at a cached path to
# avoid re-downloading the schema set every run); empty by default for local use.
KUBECONFORM_CACHE    ?=
KUBECONFORM_FLAGS    := -strict -summary -kubernetes-version $(MANIFEST_K8S_VERSION) -ignore-missing-schemas \
                        $(if $(KUBECONFORM_CACHE),-cache $(KUBECONFORM_CACHE),)
YAMLLINT_PATHS       := charts/actions-gateway cmd/agc/config cmd/gmc/config
# Standalone manifests not emitted by `kustomize build cmd/gmc/config/default`
# (they are opt-in or separately-applied resources). kustomization.yaml files
# and strategic-merge/JSON6902 patch fragments are deliberately NOT listed: they
# are not standalone manifests (no kind/apiVersion, or a bare patch array) and
# kubeconform cannot parse them — their validity is proven when `kustomize
# build` succeeds and by yamllint.
MANIFEST_STANDALONE  := cmd/agc/config/rbac/role.yaml \
                        cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml \
                        cmd/gmc/config/admission-policy/namespace-psa-guard.yaml \
                        cmd/gmc/config/agc-tenant-role/agc_tenant_role.yaml \
                        cmd/gmc/config/prometheus/monitor.yaml \
                        cmd/gmc/config/samples/actions-gateway.github.com_v1alpha1_actionsgateway.yaml

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

.PHONY: polaris-scan
polaris-scan: ## Render the Helm chart and audit its Kubernetes posture with polaris (gates on danger findings; requires helm + polaris on PATH; matches the CI polaris gate)
	@command -v helm >/dev/null 2>&1 || { echo "helm not found on PATH — install: https://helm.sh/docs/intro/install/" >&2; exit 1; }
	@command -v polaris >/dev/null 2>&1 || { echo "polaris not found on PATH — install: https://polaris.docs.fairwinds.com/infrastructure-as-code/#cli" >&2; exit 1; }
	@set -euo pipefail; \
	mkdir -p "$(REPO_ROOT)/.build"; \
	echo "==> helm template $(POLARIS_CHART) (digest-pinned posture)"; \
	helm template ag "$(POLARIS_CHART)" --set-string "gmc.image.digest=$(POLARIS_RENDER_DIGEST)" > "$(POLARIS_RENDER)"; \
	echo "==> polaris audit (gate: danger findings fail; warnings reported)"; \
	polaris audit --merge-config --config "$(POLARIS_CONFIG)" --audit-path "$(POLARIS_RENDER)" \
		--format=pretty --only-show-failed-tests --set-exit-code-on-danger

.PHONY: manifest-validate
manifest-validate: ## Validate the static install manifests + Helm chart (yamllint + kubeconform + helm lint; requires yamllint, kubeconform, kustomize, helm on PATH; matches the CI manifest-validate gate)
	@command -v yamllint     >/dev/null 2>&1 || { echo "yamllint not found on PATH — install: https://yamllint.readthedocs.io/en/stable/quickstart.html" >&2; exit 1; }
	@command -v kubeconform  >/dev/null 2>&1 || { echo "kubeconform not found on PATH — install: https://github.com/yannh/kubeconform#installation" >&2; exit 1; }
	@command -v kustomize    >/dev/null 2>&1 || { echo "kustomize not found on PATH — install: https://kubectl.docs.kubernetes.io/installation/kustomize/" >&2; exit 1; }
	@command -v helm         >/dev/null 2>&1 || { echo "helm not found on PATH — install: https://helm.sh/docs/intro/install/" >&2; exit 1; }
	@set -euo pipefail; \
	echo "==> yamllint (static manifests + chart metadata)"; \
	yamllint --strict -c "$(REPO_ROOT)/.yamllint.yaml" $(YAMLLINT_PATHS); \
	echo "==> kubeconform: kustomize-rendered GMC default overlay (k8s $(MANIFEST_K8S_VERSION))"; \
	kustomize build "$(REPO_ROOT)/cmd/gmc/config/default" | kubeconform $(KUBECONFORM_FLAGS); \
	echo "==> kubeconform: standalone manifests not in the default overlay"; \
	(cd "$(REPO_ROOT)" && kubeconform $(KUBECONFORM_FLAGS) $(MANIFEST_STANDALONE)); \
	echo "==> helm lint"; \
	helm lint "$(POLARIS_CHART)"; \
	echo "==> kubeconform: Helm chart render (default values)"; \
	helm template ag "$(POLARIS_CHART)" --set-string "gmc.image.digest=$(POLARIS_RENDER_DIGEST)" \
		| kubeconform $(KUBECONFORM_FLAGS); \
	echo "==> kubeconform: Helm chart render (all optional features: ServiceMonitor + sample CR + self-signed cert)"; \
	helm template ag "$(POLARIS_CHART)" --set-string "gmc.image.digest=$(POLARIS_RENDER_DIGEST)" \
		--set metrics.serviceMonitor.enabled=true --set sampleGateway.create=true --set certManager.enabled=false \
		| kubeconform $(KUBECONFORM_FLAGS); \
	echo "OK: install artifact validates"

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
