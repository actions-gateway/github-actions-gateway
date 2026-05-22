# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.

REPO_ROOT := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
KUBEBUILDER    := $(REPO_ROOT)/.build/kubebuilder
SETUP_ENVTEST  := $(REPO_ROOT)/.build/setup-envtest

.PHONY: all build build-agc build-probe tools setup-envtest

all: build

build: build-agc build-probe

build-agc:
	go build -C cmd/agc -o ../../.build/agc .

build-probe:
	go build -C cmd/probe -o ../../.build/probe .

tools: $(CONTROLLER_GEN) $(KUBEBUILDER) $(SETUP_ENVTEST)

$(CONTROLLER_GEN):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-tools/cmd/controller-gen

$(KUBEBUILDER):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/kubebuilder/v4

setup-envtest: $(SETUP_ENVTEST)

$(SETUP_ENVTEST):
	mkdir -p $(REPO_ROOT)/.build
	cd $(REPO_ROOT)/tools && GOWORK=off go build -mod=vendor -o $@ sigs.k8s.io/controller-runtime/tools/setup-envtest
