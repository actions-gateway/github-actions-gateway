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

## End-to-end tests

E2E tests run on a local `kind` cluster, are gated by the `//go:build e2e` tag, and live under `cmd/gmc/test/e2e/`. They split into three tiers (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests)):

- **Tier A** — GMC infrastructure (no GitHub required).
- **Tier B** — AGC lifecycle against the in-cluster `test/fakegithub/` server.
- **Tier C** — real GitHub workflow dispatch (requires App credentials).

Typical local run:

```bash
make e2e-cluster        # one-time: create the kind cluster
make e2e-images         # builds gmc/agc/proxy/worker/fakegithub, loads into kind
make e2e                # runs Tier A + B
make e2e-clean          # tear down when done
```

For iterating against a single spec without re-creating the cluster, see [kind-iteration.md](kind-iteration.md). It also covers pointing AGC at fakegithub vs. real GitHub via the `AGC_EXTRA_*` env vars and using `E2E_SKIP_TEARDOWN=true` to keep state between runs.

**Local-only tests.** Two Tier-A tests are excluded from CI by the `--label-filter '!local-only'` flag:

- `E2E_GMC_HPA_ScalesUpUnderLoad` — needs sustained CPU load to trigger autoscaling; flaky on 2-vCPU runners where the load generator and proxy pods compete for the same cores.
- `E2E_GMC_PDB_PreventsEvictionBelowMinAvailable` — uses `kubectl drain`, whose eviction timing becomes flaky under CPU contention.

Both pass reliably on a local machine with more cores. To run them locally, drop the label filter from the `make e2e` invocation or invoke `ginkgo` directly.

**Tier C.** Set `E2E_GITHUB_APP_ID`, `E2E_GITHUB_APP_INSTALLATION_ID`, `E2E_GITHUB_APP_PRIVATE_KEY`, `E2E_GITHUB_ORG`, and `E2E_GITHUB_REPO` in the environment, then run `make e2e` (Tier C specs skip themselves at runtime when any variable is missing). The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

## CI workflows and scripts

When adding or editing CI workflows and scripts, use the per-module commands listed under **Testing** in [`CLAUDE.md`](../../CLAUDE.md) — that is the canonical list (one `(cd <module> && go test ./...)` per module). Never use `go test ./...` from the repo root in CI — it does not work with the Go workspace layout.
