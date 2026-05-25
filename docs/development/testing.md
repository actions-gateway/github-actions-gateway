# Agent reference: Testing

## Integration tests

Integration tests require `KUBEBUILDER_ASSETS` to be set. Build the vendored `setup-envtest` binary first:

```bash
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.30.x --bin-dir /tmp/envtest-bins -p path)
(cd cmd/agc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
(cd cmd/gmc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
```

GMC unit tests also require `KUBEBUILDER_ASSETS` for the envtest suite embedded in the non-integration package. If `(cd cmd/gmc && go test ./...)` fails with a missing assets error, set the variable as above before running.

## CI workflows and scripts

When adding or editing CI workflows and scripts, use the same per-module commands as in `CLAUDE.md`. Never use `go test ./...` from the repo root in CI — it does not work with the Go workspace layout.

Per-module commands for reference:

```bash
(cd broker     && go test ./...)    # broker module
(cd githubapp  && go test ./...)    # githubapp module
(cd cmd/agc   && go test ./...)     # AGC module
(cd cmd/gmc   && go test ./...)     # GMC module
(cd cmd/probe && go test ./...)     # probe module
(cd cmd/proxy && go test ./...)     # proxy module
(cd cmd/worker && go test ./...)    # worker module
```
