# Makefile for the Actions Gateway Controller (AGC).
# Run `make` (or `make help`) for the list of available targets.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen

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
test: ## Run unit tests
	go test -race ./...

.PHONY: generate
generate: manifests ## Alias for `manifests`

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate CRD/RBAC manifests from kubebuilder markers
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) crd rbac:roleName=agc-role paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools
