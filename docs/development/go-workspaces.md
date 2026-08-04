# Agent reference: Go workspaces, vendoring, and worktrees

## Workspace layout

This repo uses a `go.work` workspace with no root-level Go module. The workspace modules are listed below in dependency order (leaf first). The **Internal deps** column lists the other workspace modules each one imports via `replace` directives:

| Directory | Module path | Internal deps |
|---|---|---|
| `api/` | `github.com/actions-gateway/github-actions-gateway/api` | — |
| `githubapp/` | `github.com/actions-gateway/github-actions-gateway/githubapp` | — |
| `broker/` | `github.com/actions-gateway/github-actions-gateway/broker` | `githubapp` |
| `scaleset/` | `github.com/actions-gateway/github-actions-gateway/scaleset` | `githubapp` |
| `cmd/probe/` | `github.com/actions-gateway/github-actions-gateway/probe` | `broker`, `githubapp`, `scaleset` |
| `cmd/agc/` | `github.com/actions-gateway/github-actions-gateway/agc` | `api`, `broker`, `githubapp` |
| `cmd/gmc/` | `github.com/actions-gateway/github-actions-gateway/gmc` | `api`, `broker`, `githubapp`, `agc` |
| `cmd/proxy/` | `github.com/actions-gateway/github-actions-gateway/proxy` | — |
| `cmd/worker/` | `github.com/actions-gateway/github-actions-gateway/worker` | — |
| `test/fakegithub/` | `github.com/actions-gateway/github-actions-gateway/fakegithub` | `broker` |

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
fakegithub ─► broker   (broker/brokerstub only — the stdlib-only shared-double core)
gmc ─► agc
agc, gmc ─► api
scaleset ─► githubapp

proxy, worker   (standalone — no internal deps)
```

`scaleset` (the GAG-owned runner-scale-set protocol client, Q264 Option E) is a
`broker`-style leaf that depends only on `githubapp`; it has no importer yet —
the AGC will import it in Q264 P3 when the scale-set acquisition tier lands.

`githubapp` (GitHub App auth/JWT) and `broker` (the GitHub broker client) are the shared libraries; the `cmd/*` binaries depend *on* them, never the reverse. `api` (the shared v2 API kinds) is a third leaf both controllers depend on. The one cross-binary edge is `gmc → agc` (the Gateway Manager Controller imports the Actions Gateway Controller's API types to provision instances); the `api` leaf exists precisely so the AGC can read the GMC-group v2 kinds without an `agc → gmc` back-edge that would close a cycle. **Keep edges pointing toward the leaves:** a new import that makes `githubapp`, `broker`, or `api` depend on a `cmd/*` module, or makes `agc` depend on `gmc`, inverts the layering and should be restructured instead. Go's compiler rejects outright *cycles* for free; this graph captures the intended *direction* so a technically-legal-but-wrong edge is caught in review. `scripts/go/go-work-tidy.sh` derives this same order at runtime (via `go list -m all`) to tidy modules leaf-first.

All runtime modules share a single `vendor/` at the repo root, produced by `go work vendor` and committed to git. Docker builds and CI rely on this — they invoke `go build` with `-mod=vendor` auto-selected (no proxy.golang.org during build).

`test/fakegithub` is an HTTP stub used by fake-GitHub e2e tests, listed in `go.work` so its packages are covered by `go work vendor`. It imports one internal package — `broker/brokerstub`, the shared session/credential mechanics every in-repo broker double now builds on (Q368) — which is deliberately standard-library-only, so the fakegithub binary links no third-party code and its distroless, Trivy-scanned image stays lean. Keep `broker/brokerstub` dependency-free for that reason: an import of the `broker` client (githubapp/JWT/Prometheus) would enlarge the scanned surface.

`tools/` has its own separate `vendor/` (`tools/vendor/`) for the kubebuilder/controller-gen toolchain. That's independent and managed by `make tools`. Do not merge it into the workspace vendor. It holds pinned third-party tools only — first-party Go tooling gets its own module, per [First-party Go tooling stays outside the workspace](#first-party-go-tooling-stays-outside-the-workspace).

When you need to *read* a dependency's source, read the committed `vendor/` (or `tools/vendor/`) tree, not the module cache — `~/go/pkg/mod` sits outside the worktree (so workspace-guard prompts on it) *and* may hold a different version than the `-mod=vendor` build actually uses.

### Why replace directives are still present

`broker`, `githubapp`, and the `cmd/*` modules depend on each other using `replace` directives in their individual `go.mod` files, even though the workspace `use` directives already provide local overrides at build time. This is necessary because `go mod tidy` and `go work sync` validate that required versions are resolvable; the zero pseudo-version placeholder (`v0.0.0-00010101000000-000000000000`) is only valid alongside a `replace` directive. Do not remove those `replace` lines — they are load-bearing for tidy.

## First-party Go tooling stays outside the workspace

Repo tooling written in Go — gate implementations, linters, report generators — goes in `devtools/`, a module deliberately **not** listed in `go.work`.

| Directory | Holds | In `go.work`? |
|---|---|---|
| `tools/` | pinned **third-party** build tools (`tools.go` blank imports, built by `make tools` into `.build/`) | no |
| `devtools/` | **first-party** Go programs backing `make` targets | no |

Packages inside `devtools/` are grouped by the gate that runs them, mirroring `scripts/`: `devtools/ci/pathfilters/` is the Go half of a `ci/` gate whose entry point stays [`scripts/ci/check-path-filters.sh`](../../scripts/ci/check-path-filters.sh). That keeps "which gate runs this?" answerable from the path, which is what the CI path filters need — they are plain prefix globs.

A gate needs no separate compile step in its Makefile target: `go build`/`go run` cache, so the first invocation pays the compile and later ones do not. Which of the two to use depends on how often the gate calls the program:

- **Called once** — `go run` from the module directory, like `tools/`: `(cd devtools && go run ./ci/pathfilters …)`. Warm that costs ~42ms.
- **Called in a loop** — build once into the gitignored `.build/` and exec the binary, which costs ~17ms a call against ~42ms for a `go run` that re-links every time. `check-path-filters.sh` does this in `ensure_pathfilters`; it invokes the extractor dozens of times per run.
- **Its exit status is the gate's verdict** — build and exec even when called once. `go run` adds an `exit status 1` line of its own to stderr on top of the program's findings, and suppressing that would suppress the toolchain's compile errors with it. `check-doc-links.sh` builds for this reason. Put the binary beside the source it is built from, not under the tree being checked: a test suite points the gate at a throwaway repo.

### Why it stays out of the workspace, and out of `scripts/`

[`scripts/ci/check-path-filters.sh`](../../scripts/ci/check-path-filters.sh) fails any workspace-covering filter that does not match every `go.work` module, and that set includes `e2e-test.yml:e2e` and `security-scan.yml:code`. A module in the workspace therefore drags every change to it through an image bake and an e2e cluster — an unbounded per-PR cost to pay for a docs linter.

It cannot live under `scripts/` either. A Go module brings a `vendor/` tree, and vendored dependencies ship shell scripts (41 across the current `vendor/` and `tools/vendor/` — `zap/checklicense.sh`, `kubebuilder/test_e2e.sh`, and others). [`scripts/ci/shellcheck-scripts.sh`](../../scripts/ci/shellcheck-scripts.sh) lints `scripts/**/*.sh` recursively, so third-party shell would land in the shellcheck gate.

### Wiring a new first-party module

Outside `go.work` the Go gates do not see the module: `go-test.sh`, `go-lint.sh`, `coverage.sh` and `go-vulncheck.sh` all iterate `workspace_modules()` (`go work edit -json`). What they loop with `GOWORK=off` is `firstparty_nonworkspace_modules()` in [`scripts/lib/common.sh`](../../scripts/lib/common.sh), and **that needs no edit** — it discovers every tracked `go.mod` outside `go.work`, excluding vendored trees and `tools/`. It was a hand-maintained list until Q670, where the cost of a gate that covers a module only on remembering to widen it was measured directly: forgetting is silent, and every gate stays green. What still needs doing:

1. Vendor it in [`scripts/go/vendor-sync.sh`](../../scripts/go/vendor-sync.sh), beside the existing `tools/` line: `(cd devtools && GOWORK=off go mod vendor)`, and add the same tree to [`scripts/go/vendor-check.sh`](../../scripts/go/vendor-check.sh) so the integrity gate actually diffs it.
2. Add `<module>/vendor/**` to the `vendor` path filter, so that gate re-runs when the tree changes.
3. Classify `<module>/**` in a **narrow** CI path filter — the lint/scripts jobs, never e2e — so `check-path-filters.sh` assertion 1 passes.
4. Add a pointer from [`scripts/README.md`](../../scripts/README.md) so the gate map stays in one place.

**It owes no `THIRD-PARTY-NOTICES` entry.** That file is generated from the repo-root `vendor/` tree alone, because attribution is triggered by distributing a binary and these modules are never shipped — the same reason `tools/vendor/` is excluded. Adding a dependency here is therefore not a notices change. Scope rule and the source-tree/SBOM distinctions: [building.md](building.md#what-it-covers--and-why-build-time-tooling-does-not).

Each gate needs its own `GOWORK=off` pass rather than a widened module list: they run a single workspace-wide invocation (`go test` over every module pattern, `gofmt -l` over every module dir), and that invocation resolves against `go.work`, which by construction does not list this module.

**`coverage.sh` is a partial exception.** Its ratchet builds one profile from the workspace build list and filters it per module, so a non-workspace module carries no baseline row and no floor — widening that means merging a second profile, which has not been worth it for a module of this size. It does still *run* those tests, unmeasured, because `make check` calls `cover-check` in place of `make test`: without that pass the fast gate would never execute them, and they would only run under `make test`/`make test-race`.

`scripts/go/check-go-version.sh` needs no change: it already asserts a single `go` directive across every `go.mod`, so a new module inherits the check.

## Changing dependencies

When you change any module's `go.mod` (add, upgrade, or remove a dep):

1. Run `scripts/go/go-work-tidy.sh` to tidy all modules in dependency order.
2. Run `go work sync` to sync the workspace build list.
3. Run `go work vendor` at the repo root to update the shared `vendor/`.
4. Commit the `go.mod`, `go.sum`, and `vendor/` changes together in the same commit so they stay in sync.

`make vendor-sync` (→ `scripts/go/vendor-sync.sh`) runs steps 1–3 plus the `THIRD-PARTY-NOTICES` regen in one shot, so you can do the whole sync with a single command and then commit the result. It is the same remedy the [Dependabot auto-sync workflow](#dependabot-go-bumps-are-auto-synced) runs.

If the change **added, removed, or re-pointed an inter-module `replace` edge** (or added/deleted a workspace module), also update the module table's **Internal deps** column and the **Dependency direction** graph in [Workspace layout](#workspace-layout) above — those are maintained by hand and will otherwise drift.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that produces state that conflicts with the workspace vendor. `scripts/go/go-work-tidy.sh` handles correct ordering across modules so you don't have to.

### Module-file tidiness is gated in CI

`go mod tidy` is the canonical normaliser for each module's `go.mod`/`go.sum`: it adds the missing entries (including a `/go.mod` hash row for every module in the build graph) and drops the unused ones. If a committed `go.sum` is not in that canonical shape, step 1 above re-adds those rows and step 2 re-resolves any stale indirect `require` versions — producing a spurious diff that contributors keep reverting (Q94). The `tidy-check` CI job (`make tidy-check` → `scripts/go/go-tidy-check.sh`) re-runs steps 1–2 and fails on any drift in `go.mod`/`go.sum`/`go.work.sum`, so the committed module files stay tidy-canonical. Run `make tidy-check` locally to reproduce the gate; like `vendor-check` it can need network on a cold cache, so it is intentionally **not** part of the fast `make check` gate. The remedy for a failure is steps 1–2 + commit, never an exemption.

**Editing imports is a dependency change.** Adding the first import of a module `go.mod` records as `// indirect` promotes it to a direct `require`; dropping the last import demotes or removes it. Either way the module files are untidy with nothing but a `.go` file in the diff, so run steps 1–2 after an import edit, not just after a version bump. The `tidy-check` job's path filter watches `**/*.go` for exactly this reason (Q545): before it did, PR #890 merged an untidy `cmd/gmc/go.mod` and the gate then failed on `main` — and on every branch cut from it — until #907 re-tidied six days later.

### Vendor integrity is gated in CI

`go build -mod=vendor` checks only `vendor/modules.txt` consistency — it never verifies that the vendored *source* matches the hashes in `go.sum`, so a tampered `vendor/` (or `tools/vendor/`) edit would compile into the signed release images undetected (Q126). The `vendor-check` CI job (`make vendor-check` → `scripts/go/vendor-check.sh`) re-runs the vendor flow above — which re-fetches every module verified against `go.sum` — and fails on any diff against the committed trees. Run `make vendor-check` locally to reproduce the gate; it needs network on a cold module cache (it re-fetches from the proxy), so it is intentionally **not** part of the fast `make check` gate.

A **Dependabot** `go.mod`/`go.sum` bump lands a desynced vendor tree (the bot can't run `go work vendor`), so it fails this gate by design — the fix is the follow-up vendor sync, which is now automated (see [Dependabot Go bumps are auto-synced](#dependabot-go-bumps-are-auto-synced) below), not an exemption.

### Dependabot Go bumps are auto-synced

A Dependabot Go-module PR updates one module's `go.mod`/`go.sum` but **cannot** run `go work vendor`, `go work sync`, or regenerate `THIRD-PARTY-NOTICES`. So the shared `vendor/`, `tools/vendor/`, `go.work.sum`, and `THIRD-PARTY-NOTICES` all desync and the `vendor-check`, `tidy-check`, and `license-notices` gates fail together — historically (#198) a maintainer had to hand-craft a sync commit.

The `dependabot-go-sync` workflow (`.github/workflows/dependabot-go-sync.yml`, Q111) does that for you. It triggers on every PR but its job runs only for a same-repo, Dependabot-authored PR whose branch is a Go-module update (`dependabot/go_modules/…` — the branch slug is `go_modules`, not the `gomod` package-ecosystem key in `dependabot.yml`). It runs `make vendor-sync` — the one-shot remedy that performs the whole [Changing dependencies](#changing-dependencies) flow plus the notices regen — and pushes any resulting diff back onto the Dependabot branch as a `chore(deps): sync …` commit. It no-ops cleanly (no commit) when nothing drifted, so a metadata-only bump costs one fast run.

Run the same remedy locally with `make vendor-sync` (→ `scripts/go/vendor-sync.sh`) whenever you change a dependency by hand.

### A synced branch stops auto-rebasing, and is rebased for you

Dependabot only rebases a branch it still owns, and the `chore(deps): sync …` commit marks the branch as modified by someone else. From that point Dependabot leaves it alone, so a synced PR that is not merged before `main` moves under it goes **permanently conflicting** and never self-heals on its own.

The `dependabot-rebase-stale` workflow (`.github/workflows/dependabot-rebase-stale.yml`, Q427) rescues it. On every `main` push, plus a daily safety net at 07:47 UTC and `workflow_dispatch`, it looks for open, same-repo, `CONFLICTING` Dependabot `go_modules` PRs whose branch tip is no longer Dependabot's, and rebases each one with [`scripts/ci/dependabot-rebase-stale.sh`](../../scripts/ci/dependabot-rebase-stale.sh). The branch-tip check matters: a branch the bot still owns rebases itself, and force-pushing over that would clobber it mid-flight. A run is capped at `MAX_PRS=3` PRs and names any it defers to the next run.

**It replays, it never merges.** The conflicted tree is discarded outright: the branch is reset to current `main`, every version bump the PR introduced is re-applied there with `go get`, and `make vendor-sync` regenerates the vendor trees, `go.work.sum`, and `THIRD-PARTY-NOTICES`. Bumps are recovered by diffing the `require` directives of each `go.mod` between the merge base and the branch tip, so a *grouped* PR ("bump the go-deps group across 1 directory with 5 updates") replays every one of its modules. The branch name carries only the group's hash, so it cannot be parsed for this.

Replaying rather than merging is what makes it safe. The branch's `go.mod`/`go.sum` were resolved against the older `main`, so a merge can silently *downgrade* a module `main` has since moved forward: PRs #733, #734, and #735 each conflicted only in `api/go.mod` and `api/go.sum`, yet merging any of them as-is would have reverted `golang.org/x/text` from v0.39.0 back to v0.38.0 across `api/`, `cmd/agc`, and `cmd/gmc`. Each bump is additionally guarded by Go's own signal: a `go get` that prints `downgraded` is rolled back and skipped, which also catches the transitive downgrades a direct version comparison would miss. A PR whose every bump is already on `main` is pushed nothing at all and left for a human to close.

So **never hand-resolve that conflict**, and never reach for a merge commit. Merge Go bump PRs promptly; if one goes stale, the workflow rebases it.

**Why the workflow rebases instead of commenting `@dependabot recreate`.** Because it cannot. Dependabot accepts comment commands only from **users** with push access, and rejects GitHub Apps and bots outright with "Sorry, only users with push access can use that command" ([dependabot/dependabot-core#9147](https://github.com/dependabot/dependabot-core/issues/9147), still open). A comment posted by `github-actions[bot]` with the workflow's `GITHUB_TOKEN` is ignored. This repo deliberately stores no Personal Access Token, so the automation has to do the rebase itself. A maintainer typing `@dependabot recreate` by hand still works, and remains the equivalent manual remedy.

Run it locally against the live repo with `scripts/ci/dependabot-rebase-stale.sh --list` (print what it would act on), `--dry-run` (rebase locally, push nothing), or `--bumps A/go.mod B/go.mod` (print the bumps it would extract from a pair of files).

### Both bot pushes leave the checks needing a re-trigger

The sync commit and the rebase force-push are both pushed with the workflow's default `GITHUB_TOKEN`. GitHub deliberately does **not** re-run workflows from a `GITHUB_TOKEN` push, which is what stops the bot commit from looping the sync workflow back on itself. The same rule means the required PR checks do **not** automatically re-evaluate on the new commit: they stay reported against the pre-sync or pre-rebase one. A maintainer clears it with one click either way. **Close and reopen the PR**, which re-fires the `pull_request` checks against the new head, and they pass. Using a stored Personal Access Token (PAT) instead of `GITHUB_TOKEN` would re-trigger the checks automatically, but the repo deliberately keeps no such credential, so the one-click re-trigger is the accepted trade-off. (Fork-authored Dependabot PRs are out of scope: `GITHUB_TOKEN` can't push to a fork, and this repo's Dependabot pushes branches to the repo itself, not a fork.)

## Worktrees

Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use per-module commands instead — Go finds `go.work` by walking up parent directories from `cmd/agc`, `cmd/probe`, etc. To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/actions-gateway/github-actions-gateway/agc/...
```

**No root module at the repo root.** There is no `./go.mod` and no `use .` in `go.work`. An earlier revision had a root module (`github.com/actions-gateway/github-actions-gateway`) that had to be supplied via `replace` rather than `use` to work around a Go workspace prefix-match bug (Go resolved packages under `.../agc/...` to the root module instead of `cmd/agc/` when both appeared in `use`). The root module was dropped entirely in the broker/githubapp refactor (commit `6c23b0d`), eliminating the ambiguity. Do not add `use .` or a `replace github.com/actions-gateway/github-actions-gateway => ./` back — it would reintroduce the prefix-match problem.
