# CLAUDE.md hook-rule retirement map

**Status (2026-07-20):** ✅ Complete — every mapped upstream fix shipped in an installed plugin release (workspace-guard 1.7.1, branch-guard 1.4.0, prod-guard 2.4.0, foreground-guard 0.2.0), each retirement verified per the ground rules below, and all mapped `CLAUDE.md` rules retired (Q348).

Q340 (PR #683) trimmed `CLAUDE.md`'s Hooks and Testing sections to must-act-on rules plus pointers.
Several surviving rules exist only to route around gaps in the guard plugins; upstream asks for those gaps were filed 2026-07-18.
As each ships, the mapped rule below can be deleted or shrunk further.
Piecemeal retirement is fine — take whichever rows have landed.

Two ground rules for the retirement session:

- A fix counts as landed only when it ships **in a released plugin version that is actually installed** — marketplace installs don't auto-update.
  Refresh with `claude plugin marketplace update`, then `claude plugin update <plugin>@<marketplace>`, and confirm with `claude plugin list`.
- **Verify before deleting:** reproduce the previously-prompting command once with the updated plugin (a heredoc'd Bash call, a `$(git rev-parse …)` git chain, …) and confirm no prompt/block.
  Treat the upstream issue's "closed" state as unverified until exercised.

## The map

| `CLAUDE.md` rule (Hooks/Testing sections) | Retires when | Action |
|---|---|---|
| "Avoid large heredocs in Bash commands" paragraph | workspace-guard [#83](https://github.com/karlkfi/claude-workspace-guard/issues/83) **and** branch-guard [#27](https://github.com/karlkfi/claude-branch-guard/issues/27) (heredoc-aware command parsing) both ship | ✅ **Retired (Q348, 2026-07-20):** paragraph deleted. #83 shipped in workspace-guard v1.7.0 (PR #87), #27 in branch-guard v1.4.0 (PR #31), both installed. Verified: a heredoc whose body contains command/path-like text (`rm -rf /`, `git push --force`) ran with no prompt, both standalone and inside a read-only git command |
| "Never `cd "$(git rev-parse --show-toplevel)"`" paragraph | branch-guard [#28](https://github.com/karlkfi/claude-branch-guard/issues/28) (registry of safe read-only substitutions in git chains) ships — workspace-guard ≥1.6.0 already resolves `cd` substitutions | ✅ **Retired (Q348, 2026-07-20):** rule deleted; the subshell-isolation habit sentence kept. #28 shipped in branch-guard v1.4.0 (PR #30), installed. Verified: `cd "$(git rev-parse --show-toplevel)" && git status --short` auto-approved with no prompt |
| workspace-guard bullet's "no `$VAR`, `$(...)`, or leading `~`" clause | workspace-guard [#84](https://github.com/karlkfi/claude-workspace-guard/issues/84) (resolve `~` and literal-assigned variables in file operands) ships | ✅ **Retired (Q348, 2026-07-20):** clause softened to the residual — a `$VAR` with no in-command assignment still prompts (unfixable per #84: the hook can't see shell env state). #84 shipped in workspace-guard v1.7.0 (PR #86), installed. Verified: a leading-`~` file operand and a `$(pwd)`-prefixed file operand both resolved and ran with no prompt |
| Testing bullets "Pin the target explicitly…" + "Verify the resolved target…" | the prod-guard explicit-targeting feature request (2026-07-18: deny a mutating `kubectl`/`helm`/`gcloud` lacking an explicit target flag; print the resolved target in prompts) ships | ✅ **Retired (Q348, 2026-07-20):** both bullets shrunk to one-line pointers at kind-iteration.md (doc text unchanged for contributors without the hook). The feature shipped earlier than the 2026-07-18 ask: prod-guard v2.0.0 (PR #12) denies unpinned mutations and echoes the resolved target; installed 2.4.0. Verified: an unpinned `kubectl delete --dry-run=client` denied, naming the resolved ambient context |
| Testing bullets "Never foreground-poll…" + "Slow tiers get an explicit timeout…" | the planned `foreground-guard` plugin (prompt drafted 2026-07-18) is published, installed, and this repo carries a `.claude/foreground-guard.json` with the slow-tier registry (envtest / kind e2e / `-race` minimum timeouts) | ✅ **Retired (Q348, 2026-07-20):** both bullets shrunk to one-line pointers at their testing.md sections, anchors kept. foreground-guard v0.2.0 published + installed; `.claude/foreground-guard.json` carries the slow-tier registry (`-race`/integration 600s, e2e 1800s minimum timeouts) |
| Inline `TP=…` compound-throttle recipe in the Testing throttle bullet | Q347 (go-throttle hook rewrites compound `-race` forms instead of blocking) has landed — the hook now auto-prefixes rewritable compound/redirected `-race` forms and only denies unparseable ones | ✅ **Retired (Q348, 2026-07-18):** recipe removed; the bullet now notes only the residual deny case (a `-race` form the hook can't parse, e.g. two `go` invocations) — rewritable compound/redirected forms are auto-prefixed by the hook. Verified against `scripts/agent/claude-go-throttle-hook-test.sh`. |
