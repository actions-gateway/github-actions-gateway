# Agent reference: Go workspaces, vendoring, and worktrees

## Workspace layout

This repo uses a `go.work` workspace with no root-level Go module. The workspace modules are listed below in dependency order (leaf first). The **Internal deps** column lists the other workspace modules each one imports via `replace` directives:

| Directory | Module path | Internal deps |
|---|---|---|
| `api/` | `github.com/actions-gateway/github-actions-gateway/api` | — |
| `githubapp/` | `github.com/actions-gateway/github-actions-gateway/githubapp` | — |
| `broker/` | `github.com/actions-gateway/github-actions-gateway/broker` | `githubapp` |
| `cmd/probe/` | `github.com/actions-gateway/github-actions-gateway/probe` | `broker`, `githubapp` |
| `cmd/agc/` | `github.com/actions-gateway/github-actions-gateway/agc` | `api`, `broker`, `githubapp` |
| `cmd/gmc/` | `github.com/actions-gateway/github-actions-gateway/gmc` | `api`, `broker`, `githubapp`, `agc` |
| `cmd/proxy/` | `github.com/actions-gateway/github-actions-gateway/proxy` | — |
| `cmd/worker/` | `github.com/actions-gateway/github-actions-gateway/worker` | — |
| `test/fakegithub/` | `github.com/actions-gateway/github-actions-gateway/fakegithub` | — |

The `api/` module holds the v2 (`actions-gateway.com`) `v2alpha1` API kinds shared by
both controllers. It is a pure API leaf — only `k8s.io/*` and `controller-runtime`
scheme deps, no internal deps — so both `agc` and `gmc` import it without inverting
the layering. It exists to break a would-be module cycle: the AGC's `RunnerSet`
reconciler must read the GMC-group `ActionsGateway`/`EgressProxy`, but `gmc` already
imports `agc`, so the shared kinds live in this neutral module instead of either
controller importing the other's API package (Q164). The v1 (`actions-gateway.github.com`)
kinds stay in `cmd/agc/api/v1alpha1` and `cmd/gmc/api/v1alpha1`.

### Dependency direction

The internal-dep edges form a directed acyclic graph that fans out from the two shared libraries (each arrow reads "depends on"):

```
probe ─┐
agc ───┼─► broker ─► githubapp
gmc ───┘
gmc ─► agc
agc, gmc ─► api

proxy, worker, fakegithub   (standalone — no internal deps)
```

`githubapp` (GitHub App auth/JWT) and `broker` (the GitHub broker client) are the shared libraries; the `cmd/*` binaries depend *on* them, never the reverse. `api` (the shared v2 API kinds) is a third leaf both controllers depend on. The one cross-binary edge is `gmc → agc` (the Gateway Manager Controller imports the Actions Gateway Controller's API types to provision instances); the `api` leaf exists precisely so the AGC can read the GMC-group v2 kinds without an `agc → gmc` back-edge that would close a cycle. **Keep edges pointing toward the leaves:** a new import that makes `githubapp`, `broker`, or `api` depend on a `cmd/*` module, or makes `agc` depend on `gmc`, inverts the layering and should be restructured instead. Go's compiler rejects outright *cycles* for free; this graph captures the intended *direction* so a technically-legal-but-wrong edge is caught in review. `scripts/go-work-tidy.sh` derives this same order at runtime (via `go list -m all`) to tidy modules leaf-first.

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

`make vendor-sync` (→ `scripts/vendor-sync.sh`) runs steps 1–3 plus the `THIRD-PARTY-NOTICES` regen in one shot, so you can do the whole sync with a single command and then commit the result. It is the same remedy the [Dependabot auto-sync workflow](#dependabot-go-bumps-are-auto-synced) runs.

If the change **added, removed, or re-pointed an inter-module `replace` edge** (or added/deleted a workspace module), also update the module table's **Internal deps** column and the **Dependency direction** graph in [Workspace layout](#workspace-layout) above — those are maintained by hand and will otherwise drift.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that produces state that conflicts with the workspace vendor. `scripts/go-work-tidy.sh` handles correct ordering across modules so you don't have to.

### Module-file tidiness is gated in CI

`go mod tidy` is the canonical normaliser for each module's `go.mod`/`go.sum`: it adds the missing entries (including a `/go.mod` hash row for every module in the build graph) and drops the unused ones. If a committed `go.sum` is not in that canonical shape, step 1 above re-adds those rows and step 2 re-resolves any stale indirect `require` versions — producing a spurious diff that contributors keep reverting (Q94). The `tidy-check` CI job (`make tidy-check` → `scripts/go-tidy-check.sh`) re-runs steps 1–2 and fails on any drift in `go.mod`/`go.sum`/`go.work.sum`, so the committed module files stay tidy-canonical. Run `make tidy-check` locally to reproduce the gate; like `vendor-check` it can need network on a cold cache, so it is intentionally **not** part of the fast `make check` gate. The remedy for a failure is steps 1–2 + commit, never an exemption.

### Vendor integrity is gated in CI

`go build -mod=vendor` checks only `vendor/modules.txt` consistency — it never verifies that the vendored *source* matches the hashes in `go.sum`, so a tampered `vendor/` (or `tools/vendor/`) edit would compile into the signed release images undetected (Q126). The `vendor-check` CI job (`make vendor-check` → `scripts/vendor-check.sh`) re-runs the vendor flow above — which re-fetches every module verified against `go.sum` — and fails on any diff against the committed trees. Run `make vendor-check` locally to reproduce the gate; it needs network on a cold module cache (it re-fetches from the proxy), so it is intentionally **not** part of the fast `make check` gate.

A **Dependabot** `go.mod`/`go.sum` bump lands a desynced vendor tree (the bot can't run `go work vendor`), so it fails this gate by design — the fix is the follow-up vendor sync, which is now automated (see [Dependabot Go bumps are auto-synced](#dependabot-go-bumps-are-auto-synced) below), not an exemption.

### Dependabot Go bumps are auto-synced

A Dependabot Go-module PR updates one module's `go.mod`/`go.sum` but **cannot** run `go work vendor`, `go work sync`, or regenerate `THIRD-PARTY-NOTICES`. So the shared `vendor/`, `tools/vendor/`, `go.work.sum`, and `THIRD-PARTY-NOTICES` all desync and the `vendor-check`, `tidy-check`, and `license-notices` gates fail together — historically (#198) a maintainer had to hand-craft a sync commit.

The `dependabot-go-sync` workflow (`.github/workflows/dependabot-go-sync.yml`, Q111) does that for you. It triggers on every PR but its job runs only for a same-repo, Dependabot-authored PR whose branch is a Go-module update (`dependabot/go_modules/…` — the branch slug is `go_modules`, not the `gomod` package-ecosystem key in `dependabot.yml`). It runs `make vendor-sync` — the one-shot remedy that performs the whole [Changing dependencies](#changing-dependencies) flow plus the notices regen — and pushes any resulting diff back onto the Dependabot branch as a `chore(deps): sync …` commit. It no-ops cleanly (no commit) when nothing drifted, so a metadata-only bump costs one fast run.

Run the same remedy locally with `make vendor-sync` (→ `scripts/vendor-sync.sh`) whenever you change a dependency by hand.

**Limitation — the checks need a re-trigger.** The sync commit is pushed with the workflow's default `GITHUB_TOKEN`. GitHub deliberately does **not** re-run workflows from a `GITHUB_TOKEN` push (this is what stops the bot commit from looping the sync workflow back on itself). The same rule means the required PR checks do **not** automatically re-evaluate on the synced commit — they stay reported against the pre-sync commit. A maintainer clears this with one click: **close and reopen the PR**, or comment `@dependabot recreate`, which re-fires the `pull_request` checks against the new head, and they pass. Using a stored Personal Access Token (PAT) instead of `GITHUB_TOKEN` would re-trigger the checks automatically, but the repo deliberately keeps no such credential — the one-click re-trigger is the accepted trade-off. (Fork-authored Dependabot PRs are out of scope: `GITHUB_TOKEN` can't push to a fork, and this repo's Dependabot pushes branches to the repo itself, not a fork.)

## Worktrees

Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use per-module commands instead — Go finds `go.work` by walking up parent directories from `cmd/agc`, `cmd/probe`, etc. To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/actions-gateway/github-actions-gateway/agc/...
```

**No root module at the repo root.** There is no `./go.mod` and no `use .` in `go.work`. An earlier revision had a root module (`github.com/actions-gateway/github-actions-gateway`) that had to be supplied via `replace` rather than `use` to work around a Go workspace prefix-match bug (Go resolved packages under `.../agc/...` to the root module instead of `cmd/agc/` when both appeared in `use`). The root module was dropped entirely in the broker/githubapp refactor (commit `6c23b0d`), eliminating the ambiguity. Do not add `use .` or a `replace github.com/actions-gateway/github-actions-gateway => ./` back — it would reintroduce the prefix-match problem.
