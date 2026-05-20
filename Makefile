# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder

.PHONY: all build build-agc build-probe tools

all: build

build: build-agc build-probe

build-agc:
	go build -C cmd/agc -o ../../.build/agc .

build-probe:
	go build -C cmd/probe -o ../../.build/probe .

tools: $(CONTROLLER_GEN) $(KUBEBUILDER)

$(CONTROLLER_GEN):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-tools/cmd/controller-gen

$(KUBEBUILDER):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/kubebuilder/v4
