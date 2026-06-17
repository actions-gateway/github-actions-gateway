# scripts/

Developer and CI helper scripts. All scripts follow the [repo bash conventions](../docs/development/bash-style.md): `set -euo pipefail`, `local` for function variables, `[[ ]]` conditionals, quoted expansions, `trap` cleanup for background processes — see the doc for the full list. Shared helpers (`require_cmd`, `workspace_modules`, the throttle setup) live in [lib/common.sh](lib/common.sh). Every tracked script here is linted by `make shellcheck` ([shellcheck-scripts.sh](shellcheck-scripts.sh)).

Make-target backends — the root `Makefile` keeps recipes as thin target→script wiring so the logic is shellcheck-covered; parameters are env-overridable and documented in each script's header:

| Script | Purpose |
|---|---|
| [go-test.sh](go-test.sh) | Per-module unit tests across the Go workspace; `--race` for the race-detector gate. Backs `make test` / `make test-race`. |
| [go-lint.sh](go-lint.sh) | gofmt check + per-module golangci-lint. Backs `make lint` and the CI `lint` job. |
| [go-vulncheck.sh](go-vulncheck.sh) | Per-module govulncheck. Backs `make vulncheck` and the CI `govulncheck` job. |
| [shellcheck-scripts.sh](shellcheck-scripts.sh) | Shellcheck every tracked `scripts/*.sh` (recursive). Backs `make shellcheck`. |
| [coverage.sh](coverage.sh) | Per-module unit-test coverage + the no-regression ratchet gate. Backs `make cover`/`cover-update`/`cover-check`. |
| [trivy-scan.sh](trivy-scan.sh) | Build each image locally and scan with trivy. Backs `make trivy-scan`; mirrors the CI `trivy` matrix. |
| [polaris-scan.sh](polaris-scan.sh) | Render the Helm chart (digest-pinned) and audit posture with polaris. Backs `make polaris-scan`; mirrors the CI `polaris` job. |
| [manifest-validate.sh](manifest-validate.sh) | yamllint + kubeconform + helm lint + the fail-closed digest-pinning assertion over the install artifact. Backs `make manifest-validate`; mirrors the CI `validate` job. |
| [verify-release.sh](verify-release.sh) | Verify the cosign signatures of a published release (4 images + chart). Backs `make verify-release`. |
| [download-cosign.sh](download-cosign.sh) | Download the pinned cosign release binary for the current platform. Backs the Makefile's `$(COSIGN)` rule. |
| [gen-third-party-notices.sh](gen-third-party-notices.sh) | Regenerate (or `--check`) THIRD-PARTY-NOTICES from the committed vendor/ trees. Backs `make third-party-notices(-check)`. |
| [lint-status.sh](lint-status.sh) | Lint `docs/STATUS.md` for format rules: single-line `Last touched:`, no duplicate Queue IDs, Notes ≤250 chars. Runs in CI (`unit-test.yml`), by `make check`, and by the pre-commit hook. |
| [check-doc-links.sh](check-doc-links.sh) | GitHub-slug-aware Markdown link/anchor checker: fails on dead relative file links or `#anchors` with no matching heading slug / `<a id>`. Backs `make doc-links` and the CI `doc-links` job. |
| [local-throttle.sh](local-throttle.sh) | Detect an interactive GUI dev shell and emit a parallelism cap + low-priority QoS command prefix (empty on CI/headless), so heavy gates stay desktop-safe. |
| [queue-unblock.sh](queue-unblock.sh) | List `docs/STATUS.md` Queue items blocked on a given ID. Backs `make queue-unblock`. |

Other helpers:

| Script | Purpose |
|---|---|
| [setup.sh](setup.sh) | One-time post-clone setup: initialise Go module dependencies and verify the build. Re-run after any dependency change. |
| [go-work-tidy.sh](go-work-tidy.sh) | Run `go mod tidy` across every module in the Go workspace sequentially. See [docs/development/go-workspaces.md](../docs/development/go-workspaces.md). |
| [kind-with-registry.sh](kind-with-registry.sh) | Idempotent: start a local OCI registry and a `kind` cluster wired to use it. Foundation for Tier A/B e2e tests — see [docs/development/kind-iteration.md](../docs/development/kind-iteration.md). |
| [start-registry.sh](start-registry.sh) | Idempotent: start just the local OCI registry container. Backs `make e2e-registry`; also called by kind-with-registry.sh. |
| [run-parallel.sh](run-parallel.sh) | Run multiple commands in parallel with labeled, real-time output. Useful for running per-module tests concurrently. |
| [probe-live-run.sh](probe-live-run.sh) | End-to-end setup and execution of the Milestone 1 wire-protocol probe against a real GitHub App installation. |
| [probe-investigations-cd.sh](probe-investigations-cd.sh) | Runs Milestone 1 Investigations C and D against real GitHub. |
| [pull-image-with-retry.sh](pull-image-with-retry.sh) | `docker pull <image-ref>` with bounded retries (`PULL_RETRY_ATTEMPTS`/`PULL_RETRY_DELAY` env, default 5×5s). Absorbs transient registry timeouts/429s in-step. Used by the e2e and security-scan workflows to pre-pull the buildkit builder and mirror the curl test image. |
| [retry.sh](retry.sh) | Run an arbitrary command with bounded retries and linear backoff. Used by the publish workflow to absorb transient GHCR errors on idempotent push/sign steps. |
| [claude-go-throttle-hook.sh](claude-go-throttle-hook.sh) | Claude Code `PreToolUse` hook that rewrites a bare `go build`/`go test` to carry the local-throttle prefix (Q92). Wired in `.claude/settings.json`. |

The tracked git hooks live in [`.githooks/`](../.githooks/). Install them with `make hooks` (or `scripts/setup.sh`, which does it for you); the pre-commit hook runs a sub-second gate (gofmt on staged Go files, plus `lint-status.sh` when `docs/STATUS.md` is staged). Bypass a single commit with `git commit --no-verify`.
