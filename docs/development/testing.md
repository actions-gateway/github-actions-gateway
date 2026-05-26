# Agent reference: Testing

## Integration tests

Integration tests use envtest and are gated by the `integration` build tag. They live under `internal/controller/integration/` in both `cmd/agc` and `cmd/gmc`. Use the dedicated Makefile targets — they set `KUBEBUILDER_ASSETS` automatically:

```bash
make test-integration              # runs both cmd/agc and cmd/gmc integration tests
make -C cmd/agc test-integration   # AGC only
make -C cmd/gmc test-integration   # GMC only
```

Or manually, after building setup-envtest:

```bash
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.35 --bin-dir .build -p path)
(cd cmd/agc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
(cd cmd/gmc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
```

Unit tests (`make test` / `go test ./...`) do **not** require envtest — the integration packages are excluded by their `//go:build integration` tag.

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
