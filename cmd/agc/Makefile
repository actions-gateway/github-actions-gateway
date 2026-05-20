# Makefile for the Actions Gateway Controller (AGC)

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen

.PHONY: all
all: generate build test

.PHONY: build
build:
	go build -o ../../.build/agc .

.PHONY: test
test:
	go test -race ./...

.PHONY: generate
generate: manifests

.PHONY: manifests
manifests: $(CONTROLLER_GEN)
	GOWORK=$(CURDIR)/go.work.gen $(CONTROLLER_GEN) crd rbac:roleName=agc-role paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

$(CONTROLLER_GEN):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-tools/cmd/controller-gen
