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

.PHONY: all generate manifests build test docker-build install uninstall deploy undeploy

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

# Build the GMC Docker image. Build context is cmd/gmc/ (self-contained module).
docker-build:
	docker build -f Dockerfile -t $(IMG) .

# Install CRDs into the cluster.
# Uses standalone kustomize (not kubectl -k) to support the replacements field.
# Uses --server-side to avoid the 262144-byte last-applied-configuration limit on large CRDs.
install:
	kustomize build config/crd | kubectl apply --server-side -f -

# Uninstall CRDs from the cluster.
uninstall:
	kustomize build config/crd | kubectl delete --ignore-not-found=true -f -

# Deploy the controller to the cluster.
# After kustomize apply, override the image and inject AGC/proxy image env vars.
deploy:
	kustomize build config/default | kubectl apply --server-side --force-conflicts -f -
	kubectl set image deployment/gmc-controller-manager manager=$(IMG) -n gmc-system
	kubectl set env deployment/gmc-controller-manager -c manager \
		AGC_IMAGE=$(AGC_IMAGE) PROXY_IMAGE=$(PROXY_IMAGE) \
		-n gmc-system

# Undeploy the controller from the cluster.
undeploy:
	kustomize build config/default | kubectl delete --ignore-not-found=true -f -

$(CONTROLLER_GEN):
	$(MAKE) -C $(REPO_ROOT) tools

$(ENVTEST): $(LOCALBIN)
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
