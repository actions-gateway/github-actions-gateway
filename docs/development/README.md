# docs/development/

Developer workflow references. Read the relevant doc before starting a task in that area.

| Doc | When to read |
|---|---|
| [building.md](building.md) | Building binaries — repo `Makefile` targets, `.build/` layout. |
| [backpressure.md](backpressure.md) | The developer/agent feedback loops — pre-commit hook, `make check`, CI tiers, what each mechanism catches, and a graded assessment of the result. |
| [testing.md](testing.md) | Running integration tests, editing CI workflows, picking the right test scope and tier. |
| [bash-style.md](bash-style.md) | Repo bash conventions for shell scripts — `set -euo pipefail`, quoting, traps, shellcheck-finding policy. Read before writing or editing any script. |
| [doc-update-matrix.md](doc-update-matrix.md) | Which docs to update for each kind of change — CRD fields, new behaviour, admission rules, operator-visible changes, security, module dependencies. |
| [kind-iteration.md](kind-iteration.md) | Iterating against a local `kind` cluster — image-tag caching, distroless debugging, NetworkPolicy + kube-proxy DNAT pitfalls, AGC fakegithub/real-GitHub toggle, sub-minute inner loop. Design context in [docs/design/07-test-plan.md](../design/07-test-plan.md) §7.3. |
| [networkpolicy-port-matching.md](networkpolicy-port-matching.md) | Canonical writeup of the kube-proxy DNAT vs. NetworkPolicy-port-match trap that the AGC apiserver egress rule works around. |
| [code-generation.md](code-generation.md) | Modifying CRD types under `cmd/agc/api/` or `cmd/gmc/api/` — when to regenerate, what gets regenerated, how to verify. |
| [kubernetes-conventions.md](kubernetes-conventions.md) | Project conventions for labels/annotations the operator sets — enum (not boolean-looking) values, the `actions-gateway.github.com/` key prefix, and the const-not-literal rule. Read before adding a new label, annotation, or hand-set CRD field. |
| [go-workspaces.md](go-workspaces.md) | Working across modules — workspace layout, vendoring, worktree gotchas. |
| [github-app-credentials.md](github-app-credentials.md) | Setting up GitHub App credentials for live-cluster tests (M2 kind check, M3/M4 end-to-end, Ed25519 probe, egress). |
| [technical-debt.md](technical-debt.md) | The technical-debt policy and strategy — how we classify debt, decide to fix/flag/defer/decline, what we measure, and how quality gates keep it paid down. |
| [maintaining-backlog.md](maintaining-backlog.md) | Editing [docs/STATUS.md](../STATUS.md) — Queue, Progress table, header. Rules that keep merge conflicts trivial. |
| [parallel-dispatch.md](parallel-dispatch.md) | Clearing a batch of backlog items by running several agent sessions in parallel (one session/PR per task) coordinated by a dispatcher — roles, the self-healing worker contract, the merge model, contention handling, and a pre-flight checklist. |
| [website.md](website.md) | Maintaining the docs/marketing site — MkDocs Material build + local preview, publication scope, the generated brand assets, and the progressive-enhancement JS (persona/role filter chips, per-doc audience pills) with the source markers it depends on. |
| [../design/08-glossary.md](../design/08-glossary.md) | Canonical definitions for project terms (GMC, AGC, ActionsGateway, RunnerGroup, broker protocol identifiers). |
