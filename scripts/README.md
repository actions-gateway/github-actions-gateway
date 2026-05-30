# scripts/

Developer and CI helper scripts. All scripts follow the bash conventions in [CLAUDE.md](../CLAUDE.md#bash) (`set -euo pipefail`, `local`, `[[ ]]`, quoted expansions, `trap` cleanup).

| Script | Purpose |
|---|---|
| [setup.sh](setup.sh) | One-time post-clone setup: initialise Go module dependencies and verify the build. Re-run after any dependency change. |
| [go-work-tidy.sh](go-work-tidy.sh) | Run `go mod tidy` across every module in the Go workspace sequentially. See [docs/development/go-workspaces.md](../docs/development/go-workspaces.md). |
| [kind-with-registry.sh](kind-with-registry.sh) | Idempotent: start a local OCI registry and a `kind` cluster wired to use it. Foundation for Tier A/B e2e tests — see [docs/development/kind-iteration.md](../docs/development/kind-iteration.md). |
| [run-parallel.sh](run-parallel.sh) | Run multiple commands in parallel with labeled, real-time output. Useful for running per-module tests concurrently. |
| [probe-live-run.sh](probe-live-run.sh) | End-to-end setup and execution of the Milestone 1 wire-protocol probe against a real GitHub App installation. |
| [probe-investigations-cd.sh](probe-investigations-cd.sh) | Runs Milestone 1 Investigations C and D against real GitHub. |
