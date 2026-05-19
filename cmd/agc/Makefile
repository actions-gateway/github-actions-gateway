# Makefile for the Actions Gateway Controller (AGC)

CONTROLLER_GEN_VERSION ?= v0.15.0
CONTROLLER_GEN := $(shell which controller-gen 2>/dev/null || echo "controller-gen")

.PHONY: all
all: generate build test

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test -race ./...

.PHONY: generate
generate: manifests

.PHONY: manifests
manifests:
	@if which controller-gen > /dev/null 2>&1; then \
		controller-gen crd rbac:roleName=agc-role paths="./..." output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac; \
	else \
		echo "controller-gen not found; using committed CRD manifests"; \
	fi

.PHONY: install-tools
install-tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
