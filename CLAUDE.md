# github-actions-gateway

A four-tier system for running GitHub Actions self-hosted runners in a shared Kubernetes cluster with zero idle compute. The Gateway Manager Controller (GMC) provisions isolated per-tenant gateway instances from a single `ActionsGateway` CR. Each instance is an Actions Gateway Controller (AGC) that multiplexes thousands of virtual runner sessions as goroutines — provisioning ephemeral worker pods only when a job is acquired and releasing them immediately on completion. Per-tenant egress proxy pools give each tenant isolated egress IPs for GitHub traffic. See `DESIGN.md` and `docs/design/` for full design context.

## Model selection

Use the `model-advisor` skill to assess the right model and thinking level at session start and whenever the task type shifts significantly.

## Development philosophy

Build the right thing AND build it well. Before writing any code, state the goal in one sentence and the approach in two or three. If the goal is unclear, ask one focused question rather than guessing.

Make the smallest change that achieves the goal. If you notice problems outside the current task's scope, flag them rather than fixing them:
- New near-term or long-term work → add to the Queue in `docs/STATUS.md` in priority order.
- Long-horizon non-commitments → `docs/design/appendix-g-future-enhancements.md`.

The full fix/flag/defer/decline policy, the classification taxonomy, and what we do and don't measure are in `docs/development/technical-debt.md`.

Capture knowledge durably, don't leave it in chat. When the user states a standing preference or decision, persist it in the repo (CLAUDE.md, the relevant `docs/` file, or memory) rather than applying it once and moving on. When follow-up work surfaces mid-task, record it on the Queue — including the *why* of any decision it depends on — instead of only mentioning it in the response.

Before introducing a new pattern or abstraction, check whether the codebase already solves the problem.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent. If picking the next task: run `gh pr list` first and skip any Queue item already covered by an open PR; verify 🚫 blockers are still real (grep for the blocker's deliverables — a previous session may have completed it without flipping the Queue row); mark M/L items ▶ Started in `docs/STATUS.md`.
   - **Work on a `claude/`-prefixed branch, never on `main`.** In a worktree session, do all work via the worktree path — never edit files through the parent repo's path.
   - **Check the worktree is fresh:** `git fetch origin main && git log --oneline HEAD..origin/main | head` — any output means rebase onto `origin/main` before other work. Stale worktrees cause spurious conflicts, redundant reimplementation of merged work, and outdated reads of the Queue.
   - **Treat ✅ investigation findings in plan docs as unverified until confirmed end-to-end** — actually exec the thing rather than trusting source inspection; source-reading alone has produced wrong conclusions before (PR #59).
2. **For complex tasks** — write an explicit plan to `docs/plan/` and follow it. Keep it updated as the session progresses so completed scope is verifiable at the end; revise it when new information changes the approach.
3. **After making changes** — review the diff to confirm it matches the design, is well tested, and achieves the intent. Update docs proactively per the change-type → docs mapping in `docs/development/doc-update-matrix.md` — do not wait to be asked. **A design-doc-only update is not sufficient: if a change alters what an operator does, configures, or observes (defaults, fields, failure modes, annotations, metrics, admission rejections), the operator-facing `docs/operations/` docs must be updated too.** Then update `docs/STATUS.md`: remove the completed Queue row; update the Progress table if a plan-level status changed.
4. **Commit when done** — automatically, without asking for permission (see Commits below).
5. **Open a PR when the task is finished** — after committing and pushing, open it with `gh pr create`, automatically and without asking. But first, the self-check: **"Is this ready for review?"** — yes to all of:
   - `make check` is green (plus any heavier tier the change warranted — integration/e2e, `make test-race`, `make vulncheck`, `make trivy-scan`).
   - The diff matches the design intent, is well tested, and has no stray debug code, TODOs, or unrelated changes.
   - Every doc per step 3 is updated (design **and** operator-facing), and `docs/STATUS.md` reflects the completed work.
   - The PR is scoped to one concern — unrelated work goes in its own PR.
   - The description explains *what* changed and *why*, references Queue items by bare ID (`Q44`, never `#44`), and notes how it was tested.

   If any answer is no, finish the work first — don't open a PR to "get feedback" on something you know is incomplete. If the task is too ambiguous to judge review-readiness, say so and ask.

   **Once CI attaches, confirm the path-gated heavy gates actually RAN — green is not enough.** A PR opened docs-only then given code can show all-green/`CLEAN` while integration/e2e/security never tested it; never treat such a PR as ready or merge it. Put code in the PR's first push to avoid it; if a gate is missing, `gh pr close <n> && gh pr reopen <n>` to force it. Verify/fix: [`docs/development/testing.md`](docs/development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran).

## Code standards

### Go

- Follow Go best practices for code style, naming, comments, and package organization.
- Public types, functions, and packages must have godoc comments.
- Tests must be meaningful — verify behavior, not just that the code runs.
- All go modules in the repo must use the same Go version.
- When a function starts something asynchronous, return a `<-chan struct{}` done channel so the caller controls whether and how to wait (block, select with timeout, ignore). Do not hide the channel inside a closure or call site.

### Bash

Any new or edited shell script must follow `docs/development/bash-style.md` — `set -euo pipefail`, `local` in functions, `[[ ]]`/`(( ))`, quoted expansions, cleanup `trap`s, `awk` over `sed` for variable substitutions, subshell-wrapped pipelines when capturing exit codes via `wait`. Scripts under `scripts/` are shellcheck-gated by `make check`.

## Hooks: minimizing approval prompts

Four `PreToolUse` hooks run on tool calls. Three guard Bash commands; their denial messages explain the specific fix, and the habits that avoid most prompts:

- **workspace-guard** prompts when a file read/write command (`grep`/`sed`/`cat`/`cp`/`rm`/`tee`/…) resolves a path outside the worktree. Prefer the Read/Grep/Glob tools — they bypass the guard entirely. Keep file args literal and inside the worktree: no `$VAR`, `$(...)`, or leading `~` in guarded args (the guard treats runtime-expanded tokens as outside-workspace even when they would resolve inside); temp files go in the gitignored `tmp/` at the repo root, not `/tmp` (`/dev/null` and the standard streams are exempt); don't `cd` outside the worktree (it loses cwd tracking, so later relative paths prompt); read dependency source from the committed `vendor/` and `tools/vendor/` trees, not the module cache (`~/go/pkg/mod` is outside the worktree *and* may hold a different version than the build uses). Full detail: the `workspace-guard:reduce-workspace-guard-prompts` skill.
- **branch-guard** prompts for git/edit operations on `main`/`master` and for destructive git (`reset --hard`, `clean -f`, `branch -D`, `restore <path>`, `config --global` — by design). Work on a `claude/*` branch; push the worktree's own branch (`git push -u origin HEAD`); prefer `git pull --ff-only` over bare `git pull`. Chains auto-approve only when every segment is a git/gh command, a read-only pager piped after one (`git log | head`), or an `echo`-style no-op — a real non-git segment, a file-writing redirect, or a command substitution (`$(…)`) drops the chain back to a prompt.
- **go-throttle** (`scripts/claude-go-throttle-hook.sh`) transparently rewrites a *bare* `go build`/`go test` to carry the local throttle prefix; a compound (`cd … && go test …`) or redirected form carrying `-race` is blocked with a reminder to add the prefix yourself (see Testing). No-op on CI/headless/SSH.

The fourth hook guards a different tool: **no-subagent-workers** (`scripts/claude-no-subagent-workers-hook.sh`) fires on `Agent`/`Task` spawns and *asks* (soft, not a block) when a spawn looks like a parallel-dispatch worker — own worktree, or PR-producing verbs in the prompt — steering it to a task chip instead (see `docs/development/parallel-dispatch.md`). Read-only agent types (`Explore`, `Plan`) pass untouched.

**Never `cd "$(git rev-parse --show-toplevel)"`** — the `$(…)` command substitution prompts under both branch-guard and workspace-guard. The Bash tool already starts each session at the worktree root (which *is* the git toplevel) and cwd persists between calls, so the reset is almost always a no-op anyway. Don't *assume* cwd stayed at root — make the assumption unnecessary: **isolate every directory change in a subshell** (`(cd cmd/agc && go test ./...)`) so the parent cwd can't drift and no defensive reset is ever needed. On the rare occasion you genuinely must reset cwd, `cd` to the **literal** worktree path (no `$(…)`/`$VAR`/`~`) — it's just as position-independent as recomputing the root, but prompt-free.

## Testing

[`docs/development/testing.md`](docs/development/testing.md) is the canonical reference: per-module run commands (`go test ./...` from the repo root does **not** work — Go workspace), test-tier selection, the integration/e2e tiers, and the heavier gates.

- **Run `make check` before concluding work or requesting review.** The one-command fast gate: gofmt + golangci-lint + shellcheck + `docs/STATUS.md` lint + unit tests — everything CI's `unit-test.yml` enforces except its `-race` step (reproduce that with `make test-race` when a change touches the concurrency core). A sub-second subset also runs at commit time via the pre-commit hook (`make hooks` installs it; bypass once with `git commit --no-verify`).
- **Throttle direct `go` invocations.** An unthrottled `go build`/`go test` — `-race` above all — can saturate a dev Mac and crash the GUI (WindowServer watchdog; it has actually happened — Q92). Prefer `make` targets (they auto-throttle); the go-throttle hook auto-prefixes bare commands; for compound forms add the prefix yourself:
  ```bash
  TP="../../scripts/local-throttle.sh"; (cd cmd/agc && $($TP prefix) go test -race ./...)   # TP is relative to the module dir entered by the subshell
  ```
  Details: testing.md § Resource auto-throttle.
- **Pick the tier that can observe the bug class** (testing.md § Picking the right test tier). Real-apiserver semantics (defaulting, no-op-write dedup, webhooks/CEL, `IsConflict`) need envtest — suites already exist at `internal/controller/integration/` in both `cmd/agc` and `cmd/gmc`; add to them rather than concluding none exists. Behaviors that emerge from real CNI, kube-proxy DNAT, kubelet image-pull policy, or TLS-over-tunnel need the Tier-A kind e2e tier. Iterating against a kind cluster: `docs/development/kind-iteration.md`.
- Before concluding a test failure is a code bug, check whether the problem is in the test expectations, test setup, or the code itself.
- **Flake fixes go first.** If a CI test passes on rerun without a code change, file a Queue item and move it to the top of the Queue before continuing other work — flake cost compounds across every future PR (see `docs/development/maintaining-backlog.md#flake-fixes-go-first`).

## Security principles

**Secure by default, not opt-in.** Defaults must never trade away a security property for convenience or modernity. If a new option regresses any security property — even partially, even with mitigations — the more secure option stays the default. The less secure option may be offered as an explicit opt-in with a flag or config, but must be clearly documented as a trade-off. Do not introduce security regressions as defaults without raising them explicitly and getting sign-off.

Examples of regressions that must not silently become defaults:
- Switching to a key type that loses a layer of encryption (e.g. Ed25519 agents can't decrypt RSA-OAEP session keys)
- Removing a validation, admission check, or network policy
- Relaxing a pod security profile

**Keep secrets out of environment variables.** Prefer writing a secret to a file and reading it from there, deleting the file as soon as it is no longer needed (e.g. `mktemp` + `--from-file`), over passing it through an env var. Env vars leak into process listings, logs, and child processes.

When in doubt, ask before shipping.

## Documentation conventions

Spell out acronyms on first use: write the full term first, then the acronym in parentheses — e.g. "Actions Runner Controller (ARC)". Subsequent uses may use the acronym alone.

Human-facing docs must never link to `CLAUDE.md` (or its `AGENTS.md` symlink). This file is the entrypoint for Claude/agents only; humans start at `README.md` and navigate the `docs/` tree. The dependency direction is one-way: `CLAUDE.md` may link out to `docs/`, but nothing under `docs/`, `README.md`, or `CONTRIBUTING.md` may link back to it. Canonical reference content humans need (commands, checklists, rules) lives in the `docs/` tree or `CONTRIBUTING.md`; `CLAUDE.md` keeps its own self-contained copy when it needs one.

## Commits

- Commit after each task is complete and validated — without asking; committing is automatic in this repo. Small, focused commits; Conventional Commits standard; never commit broken code or failing tests.
- **`docs/STATUS.md` changes always get their own isolated commit**, separate from code and plan-doc changes — it is high-contention across concurrent branches, and isolation keeps rebase conflicts trivial to resolve.
- Amending an unpushed commit is fine — fix up the message or staged changes without asking. Once pushed, prefer a follow-up commit; only amend + force-push (always `--force-with-lease`, never on `main`/`master`) when the user asks for it.
- After pushing, check for a PR (`gh pr view`): update an existing PR's description with `gh pr edit` to reflect new commits; if none exists and the task is finished, open one per Workflow step 5.
- If a change doesn't belong in the current PR, open a separate PR for it — parallel PRs beat bundling unrelated concerns.
- Act only on your own branch and PR. Never re-run, edit, or push to a PR or branch owned by another session; when CI fails on another session's PR, reproduce the failure locally instead.

## Agent reference docs

When working on specific tasks, read the relevant doc before starting:

| Task | Reference |
|---|---|
| Running tests or `make check`, picking a test tier/scope, editing CI workflows, the heavier gates (`test-race`, `vulncheck`, `trivy-scan`, coverage) | `docs/development/testing.md` |
| Standing up / iterating against a kind cluster | `docs/development/kind-iteration.md` (design context in `docs/design/07-test-plan.md` §7.3) |
| Go workspace / vendoring / worktrees | `docs/development/go-workspaces.md` |
| Writing or editing any shell script | `docs/development/bash-style.md` |
| Updating docs after a change — CRD fields, new behaviour, admission/validation rules, operator-visible changes, security, module dependencies | `docs/development/doc-update-matrix.md` |
| Modifying CRD types (`cmd/agc/api/`, `cmd/gmc/api/`) | `docs/development/code-generation.md` |
| Adding a label/annotation an operator sets, or a hand-set CRD field | `docs/development/kubernetes-conventions.md` |
| Building binaries | `docs/development/building.md` |
| Deciding whether to fix, flag, defer, or decline tech debt | `docs/development/technical-debt.md` |
| Picking the next task, tracking progress, adding new items | `docs/STATUS.md` — run `gh pr list` first and skip any Queue item already covered by an open PR |
| **Spawning, creating, or making any worker/agent session(s)** — including a single one — or dispatching/parallelizing work across sessions, or clearing a batch of backlog items (dispatcher + one session/PR per task). Read this **before** spawning: workers must be full Claude Code sessions (task chips), **never** Agent/Task sub-agents, and carry the Auto-fix + background conflict-watch self-healing contract. | `docs/development/parallel-dispatch.md` |
| Editing `docs/STATUS.md` (any change to the Queue, Progress table, or header) | `docs/development/maintaining-backlog.md` — Queue Notes have a **hard 250-char cap** (lint-enforced; a markdown link counts its full source length). Count before committing. |
| Security-relevant changes | `docs/design/05-security.md` + the operator half per `docs/development/doc-update-matrix.md` |
| Cutting a release, or editing the image publish/sign/SBOM pipeline (`publish.yml`) | `docs/operations/release.md` |
| Editing the docs/marketing website — MkDocs config, brand assets, or the progressive-enhancement JS | `docs/development/website.md` |
