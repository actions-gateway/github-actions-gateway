# Makefile for the Gateway Manager Controller (GMC).
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT      := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
LOCALBIN       := $(REPO_ROOT)/.build
ENVTEST        := $(LOCALBIN)/setup-envtest
YEAR           ?= $(shell date +%Y)

IMG        ?= gmc:e2e
AGC_IMAGE  ?= agc:e2e
PROXY_IMAGE ?= proxy:e2e

ENVTEST_VERSION    ?= release-0.23
ENVTEST_K8S_VERSION ?= 1.35

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
test: manifests generate ## Run unit tests with envtest
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -race $$(go list ./... | grep -v /e2e)

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=gmc-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate DeepCopy methods from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

##@ Docker

# Build the GMC Docker image. Build context is cmd/gmc/ (self-contained module).
.PHONY: docker-build
docker-build: ## Build the GMC Docker image
	docker build -f Dockerfile -t $(IMG) .

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
	kubectl set image deployment/gmc-controller-manager manager=$(IMG) -n gmc-system
	kubectl set env deployment/gmc-controller-manager -c manager \
		AGC_IMAGE=$(AGC_IMAGE) PROXY_IMAGE=$(PROXY_IMAGE) \
		-n gmc-system

.PHONY: undeploy
undeploy: ## Remove the GMC controller from the current kube-context
	kustomize build config/default | kubectl delete --ignore-not-found=true -f -

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(ENVTEST): $(LOCALBIN)
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
