# Contributing

## Prerequisites

- Go 1.26+
- Docker (for e2e tests and image builds)
- [kind](https://kind.sigs.k8s.io/) (for the local e2e cluster)
- `make`

**Optional — AI-assisted development (Claude Code):** Two skills from [`karlkfi/claude-skills`](https://github.com/karlkfi/claude-skills) are recommended:

- [`model-advisor`](https://github.com/karlkfi/claude-skills/tree/main/model-advisor) — model and thinking-level recommendations at session start and on task shifts.
- [`tech-docs-layers`](https://github.com/karlkfi/claude-skills/tree/main/tech-docs-layers) — applies the six-layer model of technical documentation when writing, editing, or restructuring docs.

```bash
# clone once, then symlink into your user-level skills directory
git clone git@github.com:karlkfi/claude-skills.git ~/workspace/claude-skills
ln -s ~/workspace/claude-skills/model-advisor    ~/.claude/skills/model-advisor
ln -s ~/workspace/claude-skills/tech-docs-layers ~/.claude/skills/tech-docs-layers
```

Two guard plugins are also recommended — `PreToolUse` hooks that keep AI-assisted work on the rails this repo expects (worktree-scoped edits, `claude/*` feature branches). Install both from within Claude Code:

```
/plugin marketplace add karlkfi/claude-workspace-guard
/plugin install workspace-guard@workspace-guard

/plugin marketplace add karlkfi/claude-branch-guard
/plugin install branch-guard@claude-branch-guard
```

- [`workspace-guard`](https://github.com/karlkfi/claude-workspace-guard) — path-aware bash permissions: prompts when a guarded file command targets a path outside the project root.
- [`branch-guard`](https://github.com/karlkfi/claude-branch-guard) — prompts before commits, pushes, or destructive git commands on a protected branch (`main`/`master`).

Restart Claude Code after installing so the hooks register (`python3` and `git` must be on your `PATH`).

Build the vendored tool binaries and install the git hooks before doing anything else:

```bash
make tools   # builds controller-gen, setup-envtest, ginkgo, kubebuilder into .build/
make hooks   # installs the tracked pre-commit hook (core.hooksPath -> .githooks)
```

`scripts/setup.sh` runs `make hooks` for you. The pre-commit hook is a sub-second gate (gofmt on staged Go files, plus the `docs/STATUS.md` format lint when that file is staged); bypass a single commit with `git commit --no-verify`.

## Design first

Before starting non-trivial work, read `DESIGN.md` and any relevant section under `docs/design/` to confirm your plan matches the design intent. The four-tier architecture has load-bearing constraints — particularly around egress isolation, zero-idle compute, and multi-tenant security boundaries — that are easy to accidentally violate with a well-intentioned shortcut.

## Building

```bash
make build       # all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc   # single binary
```

See [`docs/development/building.md`](docs/development/building.md) for the full target list and output layout.

## Testing

The repo uses a `go.work` workspace. `go test ./...` from the root does **not** work — use per-module commands:

```bash
(cd broker     && go test ./...)
(cd githubapp  && go test ./...)
(cd cmd/agc   && go test ./...)
(cd cmd/gmc   && go test ./...)
(cd cmd/probe && go test ./...)
(cd cmd/proxy && go test ./...)
(cd cmd/worker && go test ./...)
```

Integration tests require `KUBEBUILDER_ASSETS`. See [`docs/development/testing.md`](docs/development/testing.md) for setup.

## The pre-review gate

Before requesting review or opening a PR, run the one-command gate:

```bash
make check   # gofmt + golangci-lint + STATUS.md lint + unit tests
```

`make check` runs exactly what `.github/workflows/unit-test.yml` runs, so a green `make check` means a green unit-test workflow — run it locally to avoid burning CI. The slower security gates (`make vulncheck`, `make trivy-scan`) and the integration/e2e tiers are kept separate so this loop stays fast; run them when your change warrants it.

**Before merging, confirm CI actually tested the code.** Most heavy gates (integration, e2e, security scans, manifest-validate) are path-gated; a PR that was **opened as docs-only and later had code pushed** can leave those workflows *skipped* while still showing all-green and mergeable — shipping untested code to `main`. Avoid it by putting code in the PR's first push, and verify with `gh pr checks <n>` / `gh run list` that the relevant gates ran (close+reopen the PR to force them if they were skipped). See [`docs/development/testing.md`](docs/development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran).

## Linting

`make lint` runs `gofmt -s` and `golangci-lint` across every workspace module. `golangci-lint` runs `govet` internally (enabled in [`.golangci.yml`](.golangci.yml)), so it is not invoked separately. `golangci-lint` is vendored in `tools/` and built into `.build/golangci-lint`. CI runs the same gates in `.github/workflows/unit-test.yml`.

For the full e2e suite against a local kind cluster:

```bash
make e2e-up     # create cluster, build+push images, run Tier A + Tier B suites
make e2e-clean  # tear down the cluster when done
```

## Changing dependencies

When you change any module's `go.mod`:

1. Run `scripts/go-work-tidy.sh` to tidy all modules in dependency order.
2. Run `go work sync` to sync the workspace build list.
3. Run `go work vendor` at the repo root to update the shared `vendor/`.
4. Commit the `go.mod`, `go.sum`, and `vendor/` changes together in the same commit.

Do not run `go mod tidy` or `go mod vendor` inside an individual module — that conflicts with the workspace vendor. See [`docs/development/go-workspaces.md`](docs/development/go-workspaces.md) for the full vendoring discipline and worktree layout.

## Modifying CRD types

After editing types under `cmd/agc/api/` or `cmd/gmc/api/`, regenerate manifests and deepcopy code. There is a silent failure mode with RBAC markers that's worth knowing about before you hit it. See [`docs/development/code-generation.md`](docs/development/code-generation.md).

## Code standards

- Public types, functions, and packages must have godoc comments.
- Tests must verify behavior, not just that the code runs.
- Async functions return a `<-chan struct{}` done channel — callers decide whether to block, select with timeout, or ignore.
- All modules in the repo must use the same Go version.
- Shell scripts follow the repo bash conventions — see [`docs/development/bash-style.md`](docs/development/bash-style.md).

## Documentation

- Style, conventions, and maintenance for all docs live in [`docs/development/documentation-standards.md`](docs/development/documentation-standards.md) — read it before writing or restructuring a doc. The essentials:
- After a behaviour change, update every doc the change touches — the change-type → docs mapping is in [`docs/development/doc-update-matrix.md`](docs/development/doc-update-matrix.md). Design-doc updates alone are not enough when a change alters what an operator does, configures, or observes.
- Humans start at [`README.md`](README.md) and navigate the [`docs/`](docs/README.md) tree. Do **not** link to `CLAUDE.md`/`AGENTS.md` from any human-facing doc — that file is the entrypoint for AI agents only. Reference content humans need lives in `docs/` or this file.
- Spell out acronyms on first use: full term, then the acronym in parentheses — e.g. "Actions Gateway Controller (AGC)".
- Long docs (roughly 400+ lines) carry a `## Table of Contents` section after the intro, listing h2 headings (plus h3 for operator-facing docs). Anchors follow GitHub's slug rules — duplicate headings get `-1`/`-2` suffixes — so verify links against the rendered page.

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(agc): add retry budget metric for exhausted jobs
fix(gmc): correct RBAC verb for lease escalation
docs: add vendoring discipline to CONTRIBUTING
```

Keep commits small and focused. Never commit broken code or failing tests. Amending unpushed commits is fine; once pushed, prefer a follow-up commit unless a rebase is explicitly needed.

Queue items in `docs/STATUS.md` are identified by `Q`-prefixed IDs (e.g. `Q44`). Use the bare ID in commit messages and PR bodies — its `Q` prefix is what keeps GitHub from auto-linking to PR/issue 44 (`#44` would be linked, `Q44` is not).

## Security

Defaults must never trade away a security property for convenience. If a change regresses any security property — even partially — raise it explicitly before shipping. See [docs/design/05-security.md](docs/design/05-security.md) for the threat model and examples of what counts as a regression.
