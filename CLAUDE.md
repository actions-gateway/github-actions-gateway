# github-actions-gateway

A four-tier system for running GitHub Actions self-hosted runners in a shared Kubernetes cluster with zero idle compute. The Gateway Manager Controller (GMC) provisions isolated per-tenant gateway instances from a single `ActionsGateway` CR. Each instance is an Actions Gateway Controller (AGC) that multiplexes thousands of virtual runner sessions as goroutines — provisioning ephemeral worker pods only when a job is acquired and releasing them immediately on completion. Per-tenant egress proxy pools give each tenant isolated egress IPs for GitHub traffic. See `DESIGN.md` and `docs/design/` for full design context.

## Model selection

Use the `model-advisor` skill to assess the right model and thinking level at session start and whenever the task type shifts significantly.

## Development philosophy

Build the right thing AND build it well. Before writing any code, state the goal in one sentence and the approach in two or three. If the goal is unclear, ask one focused question rather than guessing.

Make the smallest change that achieves the goal. If you notice problems outside the current task's scope, flag them rather than fixing them:
- New near-term or long-term work → add to the Queue in `docs/STATUS.md` in priority order.
- Long-horizon non-commitments → `docs/design/appendix-g-future-enhancements.md`.

Capture knowledge durably, don't leave it in chat. When the user states a standing preference or decision, persist it in the repo (CLAUDE.md, the relevant `docs/` file, or memory) rather than applying it once and moving on. When follow-up work surfaces mid-task, record it on the Queue — including the *why* of any decision it depends on — instead of only mentioning it in the response.

Before introducing a new pattern or abstraction, check whether the codebase already solves the problem.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent. If picking the next task, run `gh pr list` first and skip any Queue item from `docs/STATUS.md` that is already covered by an open PR. If starting a Queue item, mark it ▶ Started there (M/L items only).
   - **Check the worktree is fresh.** New worktrees can be created from a stale local branch. Run `git fetch origin main` then compare with `git log --oneline HEAD..origin/main | head` — if there are any commits, rebase onto `origin/main` (`git rebase origin/main`) before doing any other work. Stale worktrees cause spurious conflicts, redundant reimplementation of already-merged work, and outdated reads of `docs/STATUS.md` and the Queue.
   - **Verify 🚫 blockers are still real.** A previous session may have silently completed the dependency without flipping the Queue row. Grep for the blocker's deliverables (test files, env vars, code paths) before treating the item as truly blocked. PR #59 unstuck two items whose blockers had landed weeks earlier.
   - **Investigation findings marked ✅ in a plan doc must be end-to-end verified, not just source-read.** If a §Findings block says "the X argument is Y" because of source inspection, actually exec the thing and confirm. PR #59 found `docs/plan/milestone-3.md` §11.A had the wrong Runner.Worker process invocation despite citing the right `.cs` files.
2. **For complex tasks** — write an explicit plan to `docs/plan/` and follow it. Keep it updated as the session progresses so completed scope is verifiable at the end. Revise the plan if new information changes the approach.
3. **After making changes** — review the diff to confirm it matches the design, is well tested, and achieves the intent. Update docs proactively — do not wait to be asked. Specific docs to check based on what changed:
   - **New or changed CRD fields / API surface** → `docs/design/03-api-contracts.md` (add the field with its comment block) and `docs/design/02-architecture.md` (update any prose and the metrics table if new metrics were added).
   - **New behaviour, retry logic, or operational mode** → `docs/design/02-architecture.md` (architecture prose), `docs/design/04-operational-flows.md` (flow diagrams/prose), `docs/design/07-test-plan.md` (integration test criteria), and `docs/operations/troubleshooting.md` (add a runbook section for any new failure mode an operator might observe).
   - **Security-relevant changes** → `docs/design/05-security.md`.
   - **General** → `README.md`, `CONTRIBUTING.md`, and any other `docs/` file that describes the changed behaviour. Also update `.github/workflows/` if the change affects how tests are run, what modules exist, or what build inputs CI depends on (e.g. `go-version-file`, test commands, module paths).
   - Update `docs/STATUS.md`: remove the completed Queue row; update the Progress table if a plan-level status changed (⚠️ → ✅ or a new ⚠️ item appeared).
4. **Commit when done** — once a task is complete and validated, commit with git. Keep commits small and focused. Never commit broken code or failing tests. **Always commit `docs/STATUS.md` changes in their own isolated commit**, separate from code and plan-doc changes. `docs/STATUS.md` is high-contention across concurrent branches; isolating its changes makes rebase conflicts trivial to resolve.

## Code standards

### Go

- Follow Go best practices for code style, naming, comments, and package organization.
- Public types, functions, and packages must have godoc comments.
- Tests must be meaningful — verify behavior, not just that the code runs.
- All go modules in the repo must use the same Go version.
- When a function starts something asynchronous, return a `<-chan struct{}` done channel so the caller controls whether and how to wait (block, select with timeout, ignore). Do not hide the channel inside a closure or call site.

### Bash

- Every script must start with `set -euo pipefail`.
- Use `local` for all variables inside functions.
- Use `[[ ]]` for conditionals and `(( ))` for arithmetic — never `[ ]`.
- Quote all variable expansions (`"$var"`, `"${arr[@]}"`) unless word-splitting is explicitly intended.
- When background processes need cleanup, register a `trap cleanup EXIT INT TERM` function that kills tracked PIDs.
- Prefer `awk -v name="$value" '...'` over `sed` for substitutions involving variables — `sed` delimiter and metacharacter (`/`, `&`, `\`) issues are a common source of bugs.
- When capturing the exit code of a pipeline via `wait`, wrap it in a subshell (`( cmd | other ) &`) so `$!` is the subshell's PID and `wait` reflects the pipeline result under `pipefail`, not just the last process's exit code.

## Testing

Use the per-module test commands in [`docs/development/testing.md`](docs/development/testing.md) — `go test ./...` from the repo root does not work (Go workspace). That doc is the canonical source for run commands, test-scope selection, and the integration/e2e tiers.

Before concluding a test failure is a code bug, check whether the problem is in the test expectations, test setup, or the code itself. Ensure the intent of each test matches the implementation.

**Flake fixes go first.** If a CI test passes on rerun without a code change, file a Queue item for it and move that item to the top of the Queue before continuing other work — flake cost compounds across every future PR. See [`docs/development/maintaining-backlog.md`](docs/development/maintaining-backlog.md#flake-fixes-go-first).

**Pick the right tier for the bug class.** Unit and envtest tests can't observe behaviors that emerge from real CNI, kube-proxy DNAT, kubelet image-pull policy, or TLS-over-tunnel. When a feature crosses one of those boundaries, the Tier-A kind e2e test (see [`docs/design/07-test-plan.md`](docs/design/07-test-plan.md) §7.3 and [`docs/development/testing.md`](docs/development/testing.md)) is the only thing that proves it works. PR #59 fixed 5 bugs that all unit tests passed for — a single planned-but-unimplemented Tier-A test (`E2E_GMC_TenantProvisioning_ProxyConnectWorks`) would have caught 4 of them locally.

For iterating against a real kind cluster — image-tag caching, debugging distroless pods, NetworkPolicy + kube-proxy DNAT pitfalls, AGC fakegithub/real-GitHub toggle, sub-minute inner loop — see `docs/development/kind-iteration.md`.

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

- Commit after each task is complete and validated.
- Use small, focused commits.
- Follow the Conventional Commits standard.
- Amending an unpushed commit is fine — fix up the message or staged changes before pushing without asking. Once a commit is pushed, prefer a follow-up commit; only amend + force-push (always `--force-with-lease`, never on `main`/`master`) when the user asks for it.
- After pushing, check whether a PR exists (`gh pr view`). If one does, update its description with `gh pr edit` to reflect any new commits.
- Always commit `docs/STATUS.md` changes in their own isolated commit, separate from code and plan-doc changes. `docs/STATUS.md` is high-contention across concurrent branches; isolating it makes rebase conflicts trivial to resolve.
- If a change doesn't belong in the current PR, open a separate PR for it. Working multiple PRs in parallel is fine and preferable to bundling unrelated concerns.
- Act only on your own branch and PR. Never re-run, edit, or push to a PR or branch owned by another session. When CI fails on another session's PR, reproduce the failure locally rather than touching their PR.
- Queue items in `docs/STATUS.md` have `Q`-prefixed IDs (e.g. `Q44`). Use the bare ID in commit messages and PR bodies — the `Q` is what stops GitHub from auto-linking the number to PR/issue 44.

## Agent reference docs

When working on specific tasks, read the relevant doc before starting:

| Task | Reference |
|---|---|
| Running integration tests, editing CI workflows | `docs/development/testing.md` |
| Standing up / iterating against a kind cluster | `docs/development/kind-iteration.md` (design context in `docs/design/07-test-plan.md` §7.3) |
| Go workspace / vendoring / worktrees | `docs/development/go-workspaces.md` |
| Modifying CRD types (`cmd/agc/api/`, `cmd/gmc/api/`) | `docs/development/code-generation.md` |
| Building binaries | `docs/development/building.md` |
| Picking the next task, tracking progress, adding new items | `docs/STATUS.md` — also run `gh pr list` and skip any Queue item already covered by an open PR |
| Editing `docs/STATUS.md` (any change to the Queue, Progress table, or header) | `docs/development/maintaining-backlog.md` |
| Updating API/CRD docs after a field change | `docs/design/03-api-contracts.md` |
| Updating architecture prose or metrics table | `docs/design/02-architecture.md` |
| Updating operational flow diagrams | `docs/design/04-operational-flows.md` |
| Updating integration test criteria | `docs/design/07-test-plan.md` |
| Adding a troubleshooting runbook for a new failure mode | `docs/operations/troubleshooting.md` |
| Security-relevant changes | `docs/design/05-security.md` |
