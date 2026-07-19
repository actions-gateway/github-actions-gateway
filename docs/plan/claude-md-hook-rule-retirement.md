# CLAUDE.md hook-rule retirement map

**Status (2026-07-18):** waiting on upstream plugin releases — tracked as [Q348 (Deferred)](../STATUS.md#Q348).

Q340 (PR #683) trimmed `CLAUDE.md`'s Hooks and Testing sections to must-act-on rules plus pointers. Several surviving rules exist only to route around gaps in the guard plugins; upstream asks for those gaps were filed 2026-07-18. As each ships, the mapped rule below can be deleted or shrunk further. Piecemeal retirement is fine — take whichever rows have landed.

Two ground rules for the retirement session:

- A fix counts as landed only when it ships **in a released plugin version that is actually installed** — marketplace installs don't auto-update. Refresh with `claude plugin marketplace update`, then `claude plugin update <plugin>@<marketplace>`, and confirm with `claude plugin list`.
- **Verify before deleting:** reproduce the previously-prompting command once with the updated plugin (a heredoc'd Bash call, a `$(git rev-parse …)` git chain, …) and confirm no prompt/block. Treat the upstream issue's "closed" state as unverified until exercised.

## The map

| `CLAUDE.md` rule (Hooks/Testing sections) | Retires when | Action |
|---|---|---|
| "Avoid large heredocs in Bash commands" paragraph | workspace-guard [#83](https://github.com/karlkfi/claude-workspace-guard/issues/83) **and** branch-guard [#27](https://github.com/karlkfi/claude-branch-guard/issues/27) (heredoc-aware command parsing) both ship | Delete the paragraph |
| "Never `cd "$(git rev-parse --show-toplevel)"`" paragraph | branch-guard [#28](https://github.com/karlkfi/claude-branch-guard/issues/28) (registry of safe read-only substitutions in git chains) ships — workspace-guard ≥1.6.0 already resolves `cd` substitutions | Delete the rule; keep the subshell-isolation habit sentence (it independently prevents cwd drift) |
| workspace-guard bullet's "no `$VAR`, `$(...)`, or leading `~`" clause | workspace-guard [#84](https://github.com/karlkfi/claude-workspace-guard/issues/84) (resolve `~` and literal-assigned variables in file operands) ships | Soften the clause to whatever the release still can't resolve |
| Testing bullets "Pin the target explicitly…" + "Verify the resolved target…" | the prod-guard explicit-targeting feature request (2026-07-18: deny a mutating `kubectl`/`helm`/`gcloud` lacking an explicit target flag; print the resolved target in prompts) ships | Shrink both bullets to one-line pointers at kind-iteration.md — the doc text stays for contributors without the hook |
| Testing bullets "Never foreground-poll…" + "Slow tiers get an explicit timeout…" | the planned `foreground-guard` plugin (prompt drafted 2026-07-18) is published, installed, and this repo carries a `.claude/foreground-guard.json` with the slow-tier registry (envtest / kind e2e / `-race` minimum timeouts) | Shrink both bullets to one-line pointers at their testing.md sections; keep the section anchors |
| Inline `TP=…` compound-throttle recipe in the Testing throttle bullet | Q347 (go-throttle hook rewrites compound `-race` forms instead of blocking) has landed — the hook now auto-prefixes rewritable compound/redirected `-race` forms and only denies unparseable ones | ✅ **Retired (Q348, 2026-07-18):** recipe removed; the bullet now notes only the residual deny case (a `-race` form the hook can't parse, e.g. two `go` invocations) — rewritable compound/redirected forms are auto-prefixed by the hook. Verified against `scripts/claude-go-throttle-hook-test.sh`. |
