# Contributing

## Prerequisites

- Go 1.26+
- Docker (for e2e tests and image builds)
- [kind](https://kind.sigs.k8s.io/) (for the local e2e cluster)
- `make`

Build the vendored tool binaries before doing anything else:

```bash
make tools   # builds controller-gen, setup-envtest, ginkgo, kubebuilder into .build/
```

## Design first

Before starting non-trivial work, read `DESIGN.md` and any relevant section under `docs/design/` to confirm your plan matches the design intent. The four-tier architecture has load-bearing constraints — particularly around egress isolation, zero-idle compute, and multi-tenant security boundaries — that are easy to accidentally violate with a well-intentioned shortcut.

## Building

```bash
make build       # all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc   # single binary
```

See [`docs/development/building.md`](docs/development/building.md) for the full target list and output layout.

## Testing

The repo uses a `go.work` workspace. `go test ./...` from the root does **not** work — use per-module commands:

```bash
(cd broker     && go test ./...)
(cd githubapp  && go test ./...)
(cd cmd/agc   && go test ./...)
(cd cmd/gmc   && go test ./...)
(cd cmd/probe && go test ./...)
(cd cmd/proxy && go test ./...)
(cd cmd/worker && go test ./...)
```

Integration tests require `KUBEBUILDER_ASSETS`. See [`docs/development/testing.md`](docs/development/testing.md) for setup.

For the full e2e suite against a local kind cluster:

```bash
make e2e-up     # create cluster, build+push images, run Tier A + Tier B suites
make e2e-clean  # tear down the cluster when done
```

## Changing dependencies

When you change any module's `go.mod`:

1. Run `scripts/go-work-tidy.sh` to tidy all modules in dependency order.
2. Run `go work sync` to sync the workspace build list.
3. Run `go work vendor` at the repo root to update the shared `vendor/`.
4. Commit the `go.mod`, `go.sum`, and `vendor/` changes together in the same commit.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that conflicts with the workspace vendor. See [`docs/development/go-workspaces.md`](docs/development/go-workspaces.md) for the full vendoring discipline and worktree layout.

## Modifying CRD types

After editing types under `cmd/agc/api/` or `cmd/gmc/api/`, regenerate manifests and deepcopy code. There is a silent failure mode with RBAC markers that's worth knowing about before you hit it. See [`docs/development/code-generation.md`](docs/development/code-generation.md).

## Code standards

- Public types, functions, and packages must have godoc comments.
- Tests must verify behavior, not just that the code runs.
- Async functions return a `<-chan struct{}` done channel — callers decide whether to block, select with timeout, or ignore.
- All modules in the repo must use the same Go version.

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(agc): add retry budget metric for exhausted jobs
fix(gmc): correct RBAC verb for lease escalation
docs: add vendoring discipline to CONTRIBUTING
```

Keep commits small and focused. Never commit broken code or failing tests. Amending unpushed commits is fine; once pushed, prefer a follow-up commit unless a rebase is explicitly needed.

## Security

Defaults must never trade away a security property for convenience. If a change regresses any security property — even partially — raise it explicitly before shipping. See the Security principles section in `CLAUDE.md` for examples of what counts as a regression.
