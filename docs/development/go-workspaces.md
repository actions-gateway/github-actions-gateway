# Agent reference: Go workspaces, vendoring, and worktrees

## Workspace layout

This repo uses a `go.work` workspace with no root-level Go module. The workspace modules are:

| Directory | Module path |
|---|---|
| `broker/` | `github.com/karlkfi/github-actions-gateway/broker` |
| `githubapp/` | `github.com/karlkfi/github-actions-gateway/githubapp` |
| `cmd/agc/` | `github.com/karlkfi/github-actions-gateway/agc` |
| `cmd/gmc/` | `github.com/karlkfi/github-actions-gateway/gmc` |
| `cmd/probe/` | `github.com/karlkfi/github-actions-gateway/probe` |
| `cmd/proxy/` | `github.com/karlkfi/github-actions-gateway/proxy` |
| `cmd/worker/` | `github.com/karlkfi/github-actions-gateway/worker` |
| `test/fakegithub/` | `github.com/karlkfi/github-actions-gateway/fakegithub` |

All runtime modules share a single `vendor/` at the repo root, produced by `go work vendor` and committed to git. Docker builds and CI rely on this — they invoke `go build` with `-mod=vendor` auto-selected (no proxy.golang.org during build).

`test/fakegithub` is a pure-stdlib HTTP stub used by Tier B e2e tests, listed in `go.work` so its packages are covered by `go work vendor`.

`tools/` has its own separate `vendor/` (`tools/vendor/`) for the kubebuilder/controller-gen toolchain. That's independent and managed by `make tools`. Do not merge it into the workspace vendor.

### Why replace directives are still present

`broker`, `githubapp`, and the `cmd/*` modules depend on each other using `replace` directives in their individual `go.mod` files, even though the workspace `use` directives already provide local overrides at build time. This is necessary because `go mod tidy` and `go work sync` validate that required versions are resolvable; the zero pseudo-version placeholder (`v0.0.0-00010101000000-000000000000`) is only valid alongside a `replace` directive. Do not remove those `replace` lines — they are load-bearing for tidy.

## Changing dependencies

When you change any module's `go.mod` (add, upgrade, or remove a dep):

1. Run `scripts/go-work-tidy.sh` to tidy all modules in dependency order.
2. Run `go work sync` to sync the workspace build list.
3. Run `go work vendor` at the repo root to update the shared `vendor/`.
4. Commit the `go.mod`, `go.sum`, and `vendor/` changes together in the same commit so they stay in sync.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that produces state that conflicts with the workspace vendor. `scripts/go-work-tidy.sh` handles correct ordering across modules so you don't have to.

## Worktrees

Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use per-module commands instead — Go finds `go.work` by walking up parent directories from `cmd/agc`, `cmd/probe`, etc. To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/karlkfi/github-actions-gateway/agc/...
```

**Why the worktree `go.work` uses `replace` instead of `use .`:** Two module paths share a prefix (`github.com/karlkfi/github-actions-gateway` and `.../agc`), and Go's workspace resolver doesn't apply longest-prefix matching correctly when both are listed under `use`. The root module is supplied via `replace` instead, which avoids the ambiguity. See the block comment in `go.work` for the full rationale. Do not add `.` back to the `use` block.
