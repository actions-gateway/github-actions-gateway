# Plan: Autonomous agent workflow automation

**Goal:** Let many Claude Code agents run the `docs/STATUS.md` backlog in parallel with far fewer approval prompts and far fewer manual human steps (no hand-shepherding each PR through review → CI → merge).

**Status legend:** ✅ done · ▶ in-flight (open PR) · 🔲 ready · 💤 out of repo scope

## Why

Today each backlog item is one manually-started worktree session where a human approves many permission prompts, then babysits the PR: open it, review it, wait for CI, relay failures back to the agent, and merge. The aim is to make a session run start-to-merge with the human only spot-checking outcomes.

## The four levers

| # | Lever | Status | Where |
|---|---|---|---|
| 1 | Fewer approval prompts | ▶ | container sandbox (PR #107); allowlist done |
| 2 | More parallel agents | ✅ | already worktree-per-session + headless `claude -p` |
| 3 | No PR/CI/merge babysitting | 🔲 | token minting done (PR #107); auto-merge + CI auto-fix open |
| 4 | Orchestration glue | 💤 | `/next` command + watchdog — personal Claude config |

## Lever 1 — fewer approval prompts

- ✅ **Read-only allowlist.** Scanned transcripts; the only genuine gap was `golangci-lint`, added to the gitignored `.claude/settings.local.json`. Most read-only usage (`go test`, `kubectl get`, `gh pr list`, git read-only, `grep`/`ls`/`cat`) is already auto-allowed or already listed.
- ▶ **Container sandbox (PR #107).** The real lever for autonomy: run agents with `--dangerously-skip-permissions` inside a container whose blast radius is bounded by three layers — egress firewall, credential-read deny rules, scoped credentials. See [`.devcontainer/README.md`](../../.devcontainer/README.md). Full-bypass mode consults no permission rules, so the container — not the allowlist — is the boundary.

## Lever 2 — more parallel agents

Already in place: a git worktree per session. Scaling further needs no repo change — wrap `claude -p "next"` (headless) per worktree, optionally in the PR #107 container, or use cloud agents to keep the laptop free. No open work tracked here.

## Lever 3 — eliminate PR/CI/merge babysitting

- ✅ **Scoped token minting (PR #107).** [`scripts/mint-installation-token.sh`](../../scripts/mint-installation-token.sh) mints a ≤1h, single-repo installation token with minimal permissions, reading the App key from Keychain without it ever hitting disk. Verified end-to-end.
- 🔲 **Dedicated agent identity + branch protection (go-live).** The agent must commit as its **own** least-privilege App (`contents:write`+`pull_requests:write`), **not** the `actions-gateway-test` runner App — that App lacks those permissions and carries `administration:write`, which an agent must never hold (confirmed by a 422 during testing). Steps: create the App, store its PEM under Keychain account `actions-gateway-agent`, enable branch protection on `main` (required PR + green CI, no force-push). Branch protection is the only reliable guard against a destructive push — client-side `deny` globs can't gate by target branch. → **Q62**
- 🔲 **Auto-merge + CI auto-fix.** End the relay loop: agent finishes with `gh pr merge --auto --squash` (merges when CI is green); a failed CI run dispatches the [Claude Code GitHub Action](https://github.com/anthropics/claude-code-action) to fix it on the branch; optionally a watchdog reports only genuinely-stuck PRs. Touches `.github/workflows/`. → **Q63**

## Lever 4 — orchestration glue (out of repo scope)

These live in personal Claude Code config, not this repo, so they are not Queue items:

- A deterministic `/next` slash command encoding the full loop (fetch+rebase → pick a Queue item not covered by an open PR → branch → implement → test → PR → enable auto-merge), replacing the freeform "next" prompt so every agent runs it identically.
- A `loop`/`schedule` watchdog that surfaces only PRs needing a human.

## Decisions made

- **Agent commits via a new dedicated GitHub App**, not the runner App or a PAT — preserves the ephemeral-token flow already built and keeps least privilege. (Decided 2026-06-01.)
- **Egress allowlist is Anthropic API + GitHub only** — viable because the repo vendors its Go deps, so builds/tests run offline. Drop vendoring → must add the module-proxy hosts (prefer a filtering HTTP proxy for those CDNs).

## Next concrete step

Q62: create the dedicated agent App and enable branch protection on `main`. Until then PR #107's container can be built and its firewall self-test exercised, but the agent has no identity to push as.
