# Makefile for the neutral v2 API module (github.com/.../api).
#
# This module holds the actions-gateway.com v2alpha1 kinds shared by the GMC and
# AGC controller modules. It owns only API artifacts: the DeepCopy methods and the
# five v2 CRD manifests. RBAC and webhook manifests are NOT generated here — those
# markers live on the controllers/webhooks in cmd/gmc and cmd/agc and are generated
# by those modules. See docs/development/code-generation.md.
#
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT      := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
YEAR           ?= $(shell date +%Y)

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Code generation

.PHONY: generate
generate: manifests deepcopy ## Regenerate CRD manifests and DeepCopy methods

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate the five v2 CRD manifests from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) crd paths="./..." \
		output:crd:artifacts:config=config/crd

.PHONY: deepcopy
deepcopy: $(CONTROLLER_GEN) ## Regenerate DeepCopy methods from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools
