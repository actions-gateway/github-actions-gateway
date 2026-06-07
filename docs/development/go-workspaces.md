# Agent reference: Go workspaces, vendoring, and worktrees

## Workspace layout

This repo uses a `go.work` workspace with no root-level Go module. The workspace modules are listed below in dependency order (leaf first). The **Internal deps** column lists the other workspace modules each one imports via `replace` directives:

| Directory | Module path | Internal deps |
|---|---|---|
| `githubapp/` | `github.com/actions-gateway/github-actions-gateway/githubapp` | — |
| `broker/` | `github.com/actions-gateway/github-actions-gateway/broker` | `githubapp` |
| `cmd/probe/` | `github.com/actions-gateway/github-actions-gateway/probe` | `broker`, `githubapp` |
| `cmd/agc/` | `github.com/actions-gateway/github-actions-gateway/agc` | `broker`, `githubapp` |
| `cmd/gmc/` | `github.com/actions-gateway/github-actions-gateway/gmc` | `broker`, `githubapp`, `agc` |
| `cmd/proxy/` | `github.com/actions-gateway/github-actions-gateway/proxy` | — |
| `cmd/worker/` | `github.com/actions-gateway/github-actions-gateway/worker` | — |
| `test/fakegithub/` | `github.com/actions-gateway/github-actions-gateway/fakegithub` | — |

### Dependency direction

The internal-dep edges form a directed acyclic graph that fans out from the two shared libraries (each arrow reads "depends on"):

```
probe ─┐
agc ───┼─► broker ─► githubapp
gmc ───┘
gmc ─► agc

proxy, worker, fakegithub   (standalone — no internal deps)
```

`githubapp` (GitHub App auth/JWT) and `broker` (the GitHub broker client) are the shared libraries; the `cmd/*` binaries depend *on* them, never the reverse. The one cross-binary edge is `gmc → agc` (the Gateway Manager Controller imports the Actions Gateway Controller's API types to provision instances). **Keep edges pointing toward the leaves:** a new import that makes `githubapp` or `broker` depend on a `cmd/*` module, or makes `agc` depend on `gmc`, inverts the layering and should be restructured instead. Go's compiler rejects outright *cycles* for free; this graph captures the intended *direction* so a technically-legal-but-wrong edge is caught in review. `scripts/go-work-tidy.sh` derives this same order at runtime (via `go list -m all`) to tidy modules leaf-first.

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

If the change **added, removed, or re-pointed an inter-module `replace` edge** (or added/deleted a workspace module), also update the module table's **Internal deps** column and the **Dependency direction** graph in [Workspace layout](#workspace-layout) above — those are maintained by hand and will otherwise drift.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that produces state that conflicts with the workspace vendor. `scripts/go-work-tidy.sh` handles correct ordering across modules so you don't have to.

## Worktrees

Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use per-module commands instead — Go finds `go.work` by walking up parent directories from `cmd/agc`, `cmd/probe`, etc. To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/actions-gateway/github-actions-gateway/agc/...
```

**No root module at the repo root.** There is no `./go.mod` and no `use .` in `go.work`. An earlier revision had a root module (`github.com/actions-gateway/github-actions-gateway`) that had to be supplied via `replace` rather than `use` to work around a Go workspace prefix-match bug (Go resolved packages under `.../agc/...` to the root module instead of `cmd/agc/` when both appeared in `use`). The root module was dropped entirely in the broker/githubapp refactor (commit `6c23b0d`), eliminating the ambiguity. Do not add `use .` or a `replace github.com/actions-gateway/github-actions-gateway => ./` back — it would reintroduce the prefix-match problem.
