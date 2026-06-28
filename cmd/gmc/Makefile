# Makefile for the Gateway Manager Controller (GMC).
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT      := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest
LOCALBIN       := $(REPO_ROOT)/.build
YEAR           ?= $(shell date +%Y)

# e2e image defaults reference the local registry that scripts/kind-with-registry.sh
# stands up alongside the kind cluster. Override with registry-prefixed, SHA-suffixed
# tags for e2e runs (the root Makefile sets these automatically via GMC_IMG/AGC_IMG/PROXY_IMG).
# 127.0.0.1 (not localhost): the registry is published IPv4-only, so pushing/
# resolving via localhost can hit IPv6 [::1] and fail. Matches the containerd
# mirror key in scripts/kind-with-registry.sh.
GMC_IMG     ?= 127.0.0.1:5000/gmc:e2e
AGC_IMG     ?= 127.0.0.1:5000/agc:e2e
PROXY_IMG   ?= 127.0.0.1:5000/proxy:e2e
WRAPPER_IMG ?= 127.0.0.1:5000/wrapper:e2e

ENVTEST_K8S_VERSION ?= 1.35.x

CERTMANAGER_VERSION ?= v1.20.2
_CERTMANAGER_URL    := https://github.com/cert-manager/cert-manager/releases/download/$(CERTMANAGER_VERSION)/cert-manager.yaml

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: all
all: generate build test ## Generate code/manifests, build the binary, run tests

.PHONY: build
build: ## Build the gmc binary into .build/
	go build -o $(LOCALBIN)/gmc .

.PHONY: build-migrate
build-migrate: ## Build the gag-migrate one-shot v1->v2 migration tool into .build/
	go build -o $(LOCALBIN)/gag-migrate ./migrate

.PHONY: test
test: generate ## Run unit tests (no envtest required)
	go test -race -timeout 5m ./...

.PHONY: test-integration
test-integration: manifests generate $(SETUP_ENVTEST) ## Run envtest-backed integration tests
	KUBEBUILDER_ASSETS="$(shell "$(SETUP_ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -race -timeout 5m -tags integration ./...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate DeepCopy methods from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

##@ Docker

# Build the GMC Docker image and push it to the local registry. Used for
# standalone-module workflows; the root Makefile's docker-build-gmc target is
# the canonical e2e path.
.PHONY: docker-build
docker-build: ## Build and push the GMC Docker image to the local registry
	docker buildx build --push -f Dockerfile -t $(GMC_IMG) .

##@ Cluster dependencies

.PHONY: apply-cert-manager
apply-cert-manager: ## Apply cert-manager manifests (pods start asynchronously)
	kubectl apply -f $(_CERTMANAGER_URL)

.PHONY: wait-cert-manager
wait-cert-manager: ## Wait for cert-manager deployments to be Available
	kubectl wait deployment.apps/cert-manager \
		--for=condition=Available --namespace=cert-manager --timeout=5m
	kubectl wait deployment.apps/cert-manager-webhook \
		--for=condition=Available --namespace=cert-manager --timeout=5m
	kubectl wait deployment.apps/cert-manager-cainjector \
		--for=condition=Available --namespace=cert-manager --timeout=5m

.PHONY: install-cert-manager
install-cert-manager: apply-cert-manager wait-cert-manager ## Apply cert-manager and wait for it to be ready

.PHONY: uninstall-cert-manager
uninstall-cert-manager: ## Remove cert-manager from the current kube-context
	-kubectl delete -f $(_CERTMANAGER_URL)
	-kubectl delete lease cert-manager-cainjector-leader-election cert-manager-controller \
		-n kube-system --ignore-not-found --force --grace-period=0

##@ Deploy

# The Helm chart is the SOLE cluster-install path (Q142): the artifact published
# to the OCI registry is the one this target deploys and CI e2e exercises. The
# chart owns the CRDs, RBAC, webhook + cert-manager wiring, NetworkPolicies, the
# two confinement ValidatingAdmissionPolicies (namespace-psa-guard,
# tenant-resource-guard), and the agc-tenant-role ClusterRole — everything the
# retired kustomize overlay used to apply.
HELM_RELEASE ?= actions-gateway
CHART_DIR    := $(REPO_ROOT)/charts/actions-gateway

# Deploy the GMC via the Helm chart for dev/CI iteration.
# allowFloatingImageTags=true relaxes the GMC's digest-pin requirement so the
# locally-built, tag-only images (127.0.0.1:5000/*:e2e-<sha>) are accepted; it is
# the documented DEV/TEST opt-out, never a production default. The {gmc,agc,proxy}
# image refs are split into repository + tag for the chart's image values (the
# refs always carry a tag in dev/CI). CRDs ship with the chart, so no separate
# install step is needed.
.PHONY: deploy
deploy: ## Deploy the GMC into the current kube-context via the Helm chart (dev/CI: floating image tags)
	GMC_IMG='$(GMC_IMG)'; AGC_IMG='$(AGC_IMG)'; PROXY_IMG='$(PROXY_IMG)'; WRAPPER_IMG='$(WRAPPER_IMG)'; \
	helm upgrade --install $(HELM_RELEASE) "$(CHART_DIR)" \
		--namespace gmc-system --create-namespace \
		--set allowFloatingImageTags=true \
		--set gmc.image.repository="$${GMC_IMG%:*}",gmc.image.tag="$${GMC_IMG##*:}" \
		--set agc.image.repository="$${AGC_IMG%:*}",agc.image.tag="$${AGC_IMG##*:}" \
		--set proxy.image.repository="$${PROXY_IMG%:*}",proxy.image.tag="$${PROXY_IMG##*:}" \
		--set wrapper.image.repository="$${WRAPPER_IMG%:*}",wrapper.image.tag="$${WRAPPER_IMG##*:}"

.PHONY: undeploy
undeploy: ## Remove the GMC Helm release from the current kube-context (CRDs retained: resource-policy keep)
	helm uninstall $(HELM_RELEASE) --namespace gmc-system --ignore-not-found

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(SETUP_ENVTEST):
	$(MAKE) -C $(REPO_ROOT) setup-envtest
