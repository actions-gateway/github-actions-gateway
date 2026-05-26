# github-actions-gateway

A four-tier system for running GitHub Actions self-hosted runners in a shared Kubernetes cluster with zero idle compute. The Gateway Manager Controller (GMC) provisions isolated per-tenant gateway instances from a single `ActionsGateway` CR. Each instance is an Actions Gateway Controller (AGC) that multiplexes thousands of virtual runner sessions as goroutines — provisioning ephemeral worker pods only when a job is acquired and releasing them immediately on completion. Per-tenant egress proxy pools give each tenant isolated egress IPs for GitHub traffic. See `DESIGN.md` and `docs/design/` for full design context.

## Model selection

At the start of each session, assess the work and recommend a model if the current one seems mismatched. Re-evaluate and prompt again mid-session when a trigger fires.

**Model guide:**
- **Opus 4.7** — architecture decisions, cross-cutting design changes, security analysis, complex debugging across multiple systems
- **Sonnet 4.6** — general implementation, code review, refactoring, test writing (good default)
- **Haiku 4.5** — repetitive/mechanical tasks: renaming, formatting, grep-and-replace, doc typo fixes

**Triggers for re-evaluation** — prompt with `AskUserQuestion` when:
- The task shifts from implementation to architecture or design (consider Opus)
- A bug is proving surprisingly hard to root-cause across multiple layers (consider Opus)
- A security-relevant change is discovered mid-session (consider Opus)
- The session pivots to a large batch of mechanical edits (consider Haiku)
- A new chapter starts that is clearly lighter/heavier than the previous one

When prompting, offer the three models as options, mark the recommendation, and include the `/model <id>` command the user needs to run.

## Development philosophy

Build the right thing AND build it well. Before writing any code, state the goal in one sentence and the approach in two or three. If the goal is unclear, ask one focused question rather than guessing.

Make the smallest change that achieves the goal. If you notice problems outside the current task's scope, flag them rather than fixing them:
- New near-term or long-term work → add to the Queue in `docs/STATUS.md` in priority order.
- Long-horizon non-commitments → `docs/design/appendix-g-future-enhancements.md`.

Before introducing a new pattern or abstraction, check whether the codebase already solves the problem.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent. If picking the next task, run `gh pr list` first and skip any Queue item from `docs/STATUS.md` that is already covered by an open PR. If starting a Queue item, mark it ▶ Started there (M/L items only).
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

Always use per-module test commands — `go test ./...` from the repo root does not work (Go workspace; see `docs/development/go-workspaces.md`):

```bash
(cd broker     && go test ./...)    # broker module
(cd githubapp  && go test ./...)    # githubapp module
(cd cmd/agc   && go test ./...)     # AGC module
(cd cmd/gmc   && go test ./...)     # GMC module
(cd cmd/probe && go test ./...)     # probe module
(cd cmd/proxy && go test ./...)     # proxy module
(cd cmd/worker && go test ./...)    # worker module
```

Run tests locally before pushing to a PR to avoid burning CI. Prefer the narrowest scope that covers the changes: a single module's unit tests, `-run` to target a specific test, integration tests for controller changes, or `--focus` for a targeted e2e spec. Run the full e2e suite only when changes are broad enough to warrant it.

Before concluding a test failure is a code bug, check whether the problem is in the test expectations, test setup, or the code itself. Ensure the intent of each test matches the implementation.

For integration tests and CI workflow guidance, see `docs/development/testing.md`.

## Security principles

**Secure by default, not opt-in.** Defaults must never trade away a security property for convenience or modernity. If a new option regresses any security property — even partially, even with mitigations — the more secure option stays the default. The less secure option may be offered as an explicit opt-in with a flag or config, but must be clearly documented as a trade-off. Do not introduce security regressions as defaults without raising them explicitly and getting sign-off.

Examples of regressions that must not silently become defaults:
- Switching to a key type that loses a layer of encryption (e.g. Ed25519 agents can't decrypt RSA-OAEP session keys)
- Removing a validation, admission check, or network policy
- Relaxing a pod security profile

When in doubt, ask before shipping.

## Documentation conventions

Spell out acronyms on first use: write the full term first, then the acronym in parentheses — e.g. "Actions Runner Controller (ARC)". Subsequent uses may use the acronym alone.

## Commits

- Commit after each task is complete and validated.
- Use small, focused commits.
- Follow the Conventional Commits standard.
- Amending an unpushed commit is fine — fix up the message or staged changes before pushing without asking. Once a commit is pushed, prefer a follow-up commit; only amend + force-push (always `--force-with-lease`, never on `main`/`master`) when the user asks for it.
- After pushing, check whether a PR exists (`gh pr view`). If one does, update its description with `gh pr edit` to reflect any new commits.
- Always commit `docs/STATUS.md` changes in their own isolated commit, separate from code and plan-doc changes. `docs/STATUS.md` is high-contention across concurrent branches; isolating it makes rebase conflicts trivial to resolve.
- If a change doesn't belong in the current PR, open a separate PR for it. Working multiple PRs in parallel is fine and preferable to bundling unrelated concerns.

## Agent reference docs

When working on specific tasks, read the relevant doc before starting:

| Task | Reference |
|---|---|
| Running integration tests, editing CI workflows | `docs/development/testing.md` |
| Go workspace / vendoring / worktrees | `docs/development/go-workspaces.md` |
| Modifying CRD types (`cmd/agc/api/`, `cmd/gmc/api/`) | `docs/development/code-generation.md` |
| Building binaries | `docs/development/building.md` |
| Picking the next task, tracking progress, adding new items | `docs/STATUS.md` — also run `gh pr list` and skip any Queue item already covered by an open PR |
| Updating API/CRD docs after a field change | `docs/design/03-api-contracts.md` |
| Updating architecture prose or metrics table | `docs/design/02-architecture.md` |
| Updating operational flow diagrams | `docs/design/04-operational-flows.md` |
| Updating integration test criteria | `docs/design/07-test-plan.md` |
| Adding a troubleshooting runbook for a new failure mode | `docs/operations/troubleshooting.md` |
| Security-relevant changes | `docs/design/05-security.md` |
