# tools/

Pinned versions of build- and codegen-time tool dependencies, isolated in their own Go module so they don't pollute the runtime modules' `go.sum`.

This is the standard Go [tool-dependency pattern](https://go.dev/wiki/Modules#how-can-i-track-tool-dependencies-for-a-module): [tools.go](tools.go) imports each tool under a `//go:build tools` constraint so `go mod tidy` keeps them in `go.sum` without including them in any binary build.

Currently tracked:

- `sigs.k8s.io/controller-runtime/tools/setup-envtest` — fetches the envtest binaries used by controller integration suites.
- `sigs.k8s.io/controller-tools/cmd/controller-gen` — generates CRDs, deepcopy methods, and RBAC manifests from `+kubebuilder` markers. See [docs/development/code-generation.md](../docs/development/code-generation.md).
- `sigs.k8s.io/kubebuilder/v4` — scaffolding for new controllers.

To invoke a tool at its pinned version: `(cd tools && go run sigs.k8s.io/controller-tools/cmd/controller-gen ...)`.
