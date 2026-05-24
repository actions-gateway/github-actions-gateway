# github-actions-gateway

Go module: `github.com/karlkfi/github-actions-gateway`

## Development philosophy

Alternate between product-owner and software-engineer thinking: make sure we're building the right thing AND building it well. Before starting any non-trivial task, confirm the goal, then think through the implementation.

## Workflow

1. **Before making changes** — review `DESIGN.md` and any relevant docs in `docs/` to confirm the plan matches the design intent.
2. **For complex tasks** — write an explicit plan and follow it. Revise the plan if new information changes the approach.
3. **After making changes** — review the diff to confirm it matches the design, is well tested, well documented, and achieves the intent.
4. **Commit when done** — once a task is complete and validated, commit with git. Keep commits small and focused. Never commit broken code or failing tests.

## Code standards

- Follow Go best practices for code style, naming, comments, and package organization.
- Public types, functions, and packages must have godoc comments.
- Tests must be meaningful — verify behavior, not just that the code runs.
- All go modules in the repo should use the same go version.
- When a function starts something asynchronous, return a `<-chan struct{}` done channel so the caller controls whether and how to wait (block, select with timeout, ignore). Do not hide the channel inside a closure or call site.

## Testing

- Before concluding that a test failure is a code bug, check whether the problem is in the test expectations, test setup, or the code itself.
- Ensure the intent of each test matches the implementation it's testing.
- Run tests before committing. `go test ./...` from the repo root does **not** work (see Go workspaces section below). Use the per-module commands instead:

```bash
GOWORK=off go test ./...            # root module: broker, githubapp
(cd cmd/agc   && go test ./...)     # AGC module
(cd cmd/gmc   && go test ./...)     # GMC module (requires KUBEBUILDER_ASSETS; see Integration tests below)
(cd cmd/probe && go test ./...)     # probe module
(cd cmd/proxy && go test ./...)     # proxy module
(cd cmd/worker && go test ./...)    # worker module
```

- When adding or editing CI workflows and scripts, use these same per-module commands — never `go test ./...` from the repo root.
- Integration tests require `KUBEBUILDER_ASSETS` to be set. Build the vendored `setup-envtest` binary and use it:

```bash
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.30.x --bin-dir /tmp/envtest-bins -p path)
(cd cmd/agc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
(cd cmd/gmc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
```

## Go workspaces in worktrees

This repo uses a `go.work` workspace. Worktrees (`.claude/worktrees/<name>/`) each have their own `go.work` that may differ from the root one.

**Running go commands in a worktree:** `go test ./...` from the worktree root fails because `.` is not in `go.work`'s `use` block. Use the per-module commands shown in the Testing section above (they work in the worktree too — Go finds `go.work` by walking up parent directories from `cmd/agc` and `cmd/probe`). To run a single go command against a specific module from the worktree root, set `GOWORK` explicitly:

```bash
GOWORK=/path/to/worktree/go.work go build github.com/karlkfi/github-actions-gateway/agc/...
```

**Why the worktree `go.work` uses `replace` instead of `use .`**

See the block comment in `go.work` for the full rationale. In short: two module paths share a prefix (`github.com/karlkfi/github-actions-gateway` and `.../agc`), and Go's workspace resolver doesn't apply longest-prefix matching correctly when both are listed under `use`. The root module is supplied via `replace` instead, which avoids the ambiguity. Do not add `.` back to the `use` block.

**`test/fakegithub` is its own workspace module** (`test/fakegithub/go.mod`). It's a pure-stdlib HTTP stub used by Tier B e2e tests, listed in `go.work` alongside the other runtime modules so its packages are covered by `go work vendor`.

## Go workspace vendoring

Runtime modules share a single `vendor/` at the repo root, produced by `go work vendor` and committed to git. Docker builds and CI rely on this — they invoke `go build` with `-mod=vendor` auto-selected (no proxy.golang.org during build).

**When you change any module's `go.mod`** (add, upgrade, or remove a dep):

1. Run `go work vendor` at the repo root.
2. Commit the resulting `vendor/` diff in the same commit as the `go.mod`/`go.sum` changes so they stay in sync.

Do not run `go mod vendor` inside an individual module — that produces a per-module `vendor/` that conflicts with the workspace one. Do not run `go mod tidy` without following up with `go work vendor`; tidy can prune entries that the vendor tree still references, leaving `modules.txt` out of sync.

`tools/` has its own separate `vendor/` (`tools/vendor/`) for the kubebuilder/controller-gen tool chain. That's independent and managed by `make tools`; do not merge it into the workspace vendor.

## Security principles

**Secure by default, not opt-in.** Defaults must never trade away a security property for convenience or modernity. If a new option regresses any security property — even partially, even with mitigations — the more secure option stays the default. The less secure option may be offered as an explicit opt-in with a flag or config, but must be clearly documented as a trade-off. Do not introduce security regressions as defaults without raising them explicitly and getting sign-off.

Examples of regressions that must not silently become defaults:
- Switching to a key type that loses a layer of encryption (e.g. Ed25519 agents can't decrypt RSA-OAEP session keys)
- Removing a validation, admission check, or network policy
- Relaxing a pod security profile

When in doubt, ask before shipping.

## Code generation

Whenever you modify CRD types (`cmd/agc/api/` or `cmd/gmc/api/`), run the corresponding targets for that module:

```bash
# AGC
make -C cmd/agc manifests  # regenerates CRD YAML and RBAC manifests

# GMC (two separate steps)
make -C cmd/gmc generate   # regenerates zz_generated.deepcopy.go
make -C cmd/gmc manifests  # regenerates CRD YAML and RBAC manifests
```

Both GMC steps are required. Skipping `manifests` leaves the CRD YAML out of sync with the Go types — the apiserver will silently prune unknown fields, and tests that set those fields will see the zero value instead.

`make manifests` also regenerates the `rbac/role.yaml` from `+kubebuilder:rbac` markers. Run it whenever you add or remove RBAC verbs/resources in the controller.

### RBAC marker placement

`+kubebuilder:rbac` is a **package-level** marker (controller-gen v0.21+). It must appear before the `package` declaration, not in a type's doc comment. Placing it on a struct silently produces no output — controller-gen won't warn, it will just generate nothing.

```go
// Correct — before the package declaration:
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

package controller
```

```go
// Wrong — on a type, silently ignored:

// MyReconciler reconciles things.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
type MyReconciler struct { ... }
```

The markers live at the top of `cmd/gmc/internal/controller/actionsgateway_controller.go`. Non-standard verbs (`bind`, `escalate`) are supported in `verbs=` and appear in the generated role.

## Building

All binaries are built into `.build/` at the repo root (gitignored). Use the root `Makefile`:

```bash
make build        # build all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc    # build only the AGC controller
make build-gmc    # build only the GMC controller
make build-probe  # build only the probe
make build-proxy  # build only the egress proxy
```

`cmd/worker` is a workspace module but has no dedicated root-level build target — it is built into its container image only.

Individual module Makefiles (e.g. `cmd/agc/Makefile`) also output to `.build/` via a relative path (`../../.build/`), so both `make` invocations land in the same place.

## Documentation conventions

- Spell out acronyms on first use: write the full term first, then the acronym in parentheses — e.g. "Actions Runner Controller (ARC)". Subsequent uses may use the acronym alone.

## Commits

- Commit after each task is complete and validated.
- Use small, focused commits.
- Follow the Conventional Commits standard.
- Amending an unpushed commit is fine — fix up the message or staged changes before pushing without asking. Once a commit is pushed, prefer a follow-up commit; only amend + force-push (always `--force-with-lease`, never on `main`/`master`) when the user asks for it.

## Tokens

- Minimize token use where possible.
- Predict failure scenarios and exit early to avoid waiting for long timeouts or streaming lots of output back as input.
