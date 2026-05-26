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
GMC_IMG     ?= localhost:5000/gmc:e2e
AGC_IMG     ?= localhost:5000/agc:e2e
PROXY_IMG   ?= localhost:5000/proxy:e2e

ENVTEST_K8S_VERSION ?= 1.35

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

.PHONY: test
test: manifests generate $(SETUP_ENVTEST) ## Run unit tests with envtest
	KUBEBUILDER_ASSETS="$(shell "$(SETUP_ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -race -timeout 5m $$(go list ./... | grep -v /e2e)

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

.PHONY: install-cert-manager
install-cert-manager: ## Install cert-manager into the current kube-context
	kubectl apply -f $(_CERTMANAGER_URL)
	kubectl wait deployment.apps/cert-manager \
		--for=condition=Available --namespace=cert-manager --timeout=5m
	kubectl wait deployment.apps/cert-manager-webhook \
		--for=condition=Available --namespace=cert-manager --timeout=5m
	kubectl wait deployment.apps/cert-manager-cainjector \
		--for=condition=Available --namespace=cert-manager --timeout=5m

.PHONY: uninstall-cert-manager
uninstall-cert-manager: ## Remove cert-manager from the current kube-context
	-kubectl delete -f $(_CERTMANAGER_URL)
	-kubectl delete lease cert-manager-cainjector-leader-election cert-manager-controller \
		-n kube-system --ignore-not-found --force --grace-period=0

##@ Deploy

# Install CRDs into the cluster.
# Uses standalone kustomize (not kubectl -k) to support the replacements field.
# Uses --server-side to avoid the 262144-byte last-applied-configuration limit on large CRDs.
.PHONY: install
install: ## Install GMC CRDs into the current kube-context
	kustomize build config/crd | kubectl apply --server-side -f -

.PHONY: uninstall
uninstall: ## Remove GMC CRDs from the current kube-context
	kustomize build config/crd | kubectl delete --ignore-not-found=true -f -

# Deploy the controller to the cluster.
# After kustomize apply, override the image and inject AGC/proxy image env vars.
.PHONY: deploy
deploy: ## Deploy the GMC controller into the current kube-context
	kustomize build config/default | kubectl apply --server-side --force-conflicts -f -
	kubectl set image deployment/gmc-controller-manager manager=$(GMC_IMG) -n gmc-system
	kubectl set env deployment/gmc-controller-manager -c manager \
		AGC_IMAGE=$(AGC_IMG) PROXY_IMAGE=$(PROXY_IMG) \
		-n gmc-system

.PHONY: undeploy
undeploy: ## Remove the GMC controller from the current kube-context
	kustomize build config/default | kubectl delete --ignore-not-found=true -f -

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(SETUP_ENVTEST):
	$(MAKE) -C $(REPO_ROOT) setup-envtest
