# docs/development/

Developer workflow references. Read the relevant doc before starting a task in that area.

| Doc | When to read |
|---|---|
| [building.md](building.md) | Building binaries — repo `Makefile` targets, `.build/` layout. |
| [testing.md](testing.md) | Running integration tests, editing CI workflows, picking the right test scope. |
| [kind-iteration.md](kind-iteration.md) | Iterating against a local `kind` cluster — image-tag caching, distroless debugging, NetworkPolicy + kube-proxy DNAT pitfalls, AGC fakegithub/real-GitHub toggle, sub-minute inner loop. Design context in [docs/design/07-test-plan.md](../design/07-test-plan.md) §7.3. |
| [networkpolicy-port-matching.md](networkpolicy-port-matching.md) | Canonical writeup of the kube-proxy DNAT vs. NetworkPolicy-port-match trap that the AGC apiserver egress rule works around. |
| [code-generation.md](code-generation.md) | Modifying CRD types under `cmd/agc/api/` or `cmd/gmc/api/` — when to regenerate, what gets regenerated, how to verify. |
| [go-workspaces.md](go-workspaces.md) | Working across modules — workspace layout, vendoring, worktree gotchas. |
| [github-app-credentials.md](github-app-credentials.md) | Setting up GitHub App credentials for live-cluster tests (M2 kind check, M3/M4 end-to-end, Ed25519 probe, egress). |
| [maintaining-backlog.md](maintaining-backlog.md) | Editing [docs/STATUS.md](../STATUS.md) — Queue, Progress table, header. Rules that keep merge conflicts trivial. |
