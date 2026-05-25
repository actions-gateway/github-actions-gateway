# github-actions-gateway

A four-tier system for running GitHub Actions self-hosted runners in a shared Kubernetes cluster with zero idle compute. The Gateway Manager Controller (GMC) provisions isolated per-tenant gateway instances from a single `ActionsGateway` CR. Each instance is an Actions Gateway Controller (AGC) that multiplexes thousands of virtual runner sessions as goroutines — provisioning ephemeral worker pods only when a job is acquired and releasing them immediately on completion. Per-tenant egress proxy pools give each tenant isolated egress IPs for GitHub traffic. See `DESIGN.md` and `docs/design/` for full design context.

## Development philosophy

Build the right thing AND build it well. Before writing any code, state the goal in one sentence and the approach in two or three. If the goal is unclear, ask one focused question rather than guessing.

Make the smallest change that achieves the goal. If you notice problems outside the current task's scope, flag them rather than fixing them — and if they're worth tracking, document them in `docs/todo/`. Before introducing a new pattern or abstraction, check whether the codebase already solves the problem.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent.
2. **For complex tasks** — write an explicit plan to `docs/plan/` and follow it. Keep it updated as the session progresses so completed scope is verifiable at the end. Revise the plan if new information changes the approach.
3. **After making changes** — review the diff to confirm it matches the design, is well tested, and achieves the intent. Update `README.md`, `CONTRIBUTING.md`, and any relevant docs in `docs/` if the change affects anything they describe. Also update `.github/workflows/` if the change affects how tests are run, what modules exist, or what build inputs CI depends on (e.g. `go-version-file`, test commands, module paths).
4. **Commit when done** — once a task is complete and validated, commit with git. Keep commits small and focused. Never commit broken code or failing tests.

## Code standards

- Follow Go best practices for code style, naming, comments, and package organization.
- Public types, functions, and packages must have godoc comments.
- Tests must be meaningful — verify behavior, not just that the code runs.
- All go modules in the repo must use the same Go version.
- When a function starts something asynchronous, return a `<-chan struct{}` done channel so the caller controls whether and how to wait (block, select with timeout, ignore). Do not hide the channel inside a closure or call site.

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

## Agent reference docs

When working on specific tasks, read the relevant doc before starting:

| Task | Reference |
|---|---|
| Running integration tests, editing CI workflows | `docs/development/testing.md` |
| Go workspace / vendoring / worktrees | `docs/development/go-workspaces.md` |
| Modifying CRD types (`cmd/agc/api/`, `cmd/gmc/api/`) | `docs/development/code-generation.md` |
| Building binaries | `docs/development/building.md` |
