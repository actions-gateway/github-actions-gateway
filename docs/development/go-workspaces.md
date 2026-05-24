# Agent reference: Go workspaces, vendoring, and worktrees

## Workspace layout

This repo uses a `go.work` workspace. Runtime modules share a single `vendor/` at the repo root, produced by `go work vendor` and committed to git. Docker builds and CI rely on this — they invoke `go build` with `-mod=vendor` auto-selected (no proxy.golang.org during build).

`test/fakegithub` is its own workspace module (`test/fakegithub/go.mod`). It's a pure-stdlib HTTP stub used by Tier B e2e tests, listed in `go.work` alongside the runtime modules so its packages are covered by `go work vendor`.

`tools/` has its own separate `vendor/` (`tools/vendor/`) for the kubebuilder/controller-gen toolchain. That's independent and managed by `make tools`. Do not merge it into the workspace vendor.

## Changing dependencies

When you change any module's `go.mod` (add, upgrade, or remove a dep):

1. Run `go work vendor` at the repo root.
2. Commit the resulting `vendor/` diff in the same commit as the `go.mod`/`go.sum` changes so they stay in sync.

Do not run `go mod vendor` inside an individual module — that produces a per-module `vendor/` that conflicts with the workspace one. Do not run `go mod tidy` without following up with `go work vendor`; tidy can prune entries that the vendor tree still references, leaving `modules.txt` out of sync.

## Worktrees

Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use per-module commands instead — Go finds `go.work` by walking up parent directories from `cmd/agc`, `cmd/probe`, etc. To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/karlkfi/github-actions-gateway/agc/...
```

**Why the worktree `go.work` uses `replace` instead of `use .`:** Two module paths share a prefix (`github.com/karlkfi/github-actions-gateway` and `.../agc`), and Go's workspace resolver doesn't apply longest-prefix matching correctly when both are listed under `use`. The root module is supplied via `replace` instead, which avoids the ambiguity. See the block comment in `go.work` for the full rationale. Do not add `.` back to the `use` block.
