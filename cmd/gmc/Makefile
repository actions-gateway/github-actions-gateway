REPO_ROOT      := $(shell git rev-parse --show-toplevel)
CONTROLLER_GEN := $(REPO_ROOT)/.build/controller-gen
LOCALBIN       := $(REPO_ROOT)/.build
ENVTEST        := $(LOCALBIN)/setup-envtest
YEAR           ?= $(shell date +%Y)

ENVTEST_VERSION    ?= release-0.23
ENVTEST_K8S_VERSION ?= 1.35

.PHONY: all generate manifests build test

all: generate build test

build:
	go build -o $(LOCALBIN)/gmc .

test: manifests generate
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -race $$(go list ./... | grep -v /e2e)

manifests: $(CONTROLLER_GEN)
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=gmc-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

generate: $(CONTROLLER_GEN)
	GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(ENVTEST): $(LOCALBIN)
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
