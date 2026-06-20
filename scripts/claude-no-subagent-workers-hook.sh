#!/usr/bin/env bash
#
# claude-no-subagent-workers-hook.sh — Claude Code PreToolUse hook that steers
# parallel-dispatch *workers* away from being spawned as Agent/Task sub-agents.
#
# Why this exists: docs/development/parallel-dispatch.md requires every worker to
# be a full, independent Claude Code session (a task chip via
# mcp__ccd_session__spawn_task) — never a sub-agent of the dispatcher. A
# sub-agent shares the dispatcher's session, has no worktree or branch of its
# own, cannot open and self-heal its own PR, and dies when the dispatcher's turn
# ends. CLAUDE.md and the playbook say this in prose; this hook adds a light
# enforcement nudge so the mistake is caught at spawn time.
#
# It is deliberately a *soft* gate (permissionDecision: "ask", not "deny"): the
# Agent/Task tool is used constantly for legitimate read-only work (Explore,
# Plan, research), and a stateless hook cannot know whether the user is in a
# dispatch run. So it fires only on the misuse *shape* and asks rather than
# blocks — a false positive costs one keystroke, not a broken workflow.
#
# Behaviour (fail-open everywhere — a parse/tool error never blocks a spawn):
#   * Non-Agent/Task tool                                   -> allow unchanged.
#   * subagent_type is a known read-only type (Explore,
#     Plan, claude-code-guide, statusline-setup)            -> allow unchanged.
#   * The spawn looks like a dispatch worker — it requests
#     its own worktree (isolation == "worktree") OR its
#     prompt carries PR-producing verbs (gh pr create,
#     git push/commit, "open a PR", self-heal, implement
#     Q<NN>)                                                -> ask, with a reason
#     pointing at parallel-dispatch.md and the chip mechanism.
#   * Anything else                                         -> allow unchanged.
#
# Wired up by .claude/settings.json as a PreToolUse hook on the Task|Agent
# matcher. Requires jq; if jq is missing the hook is a no-op (fail-open).
set -euo pipefail

# emit_allow_unchanged exits 0 with no output, letting the spawn proceed through
# Claude Code's normal permission flow untouched.
emit_allow_unchanged() {
	exit 0
}

# is_readonly_agent_type returns success for subagent types that only read —
# they never produce a PR and so are never the misuse this hook guards against.
is_readonly_agent_type() {
	local agent_type="$1"
	case "$agent_type" in
	Explore | Plan | claude-code-guide | statusline-setup) return 0 ;;
	*) return 1 ;;
	esac
}

# looks_like_worker returns success when the prompt carries verbs that mean the
# sub-agent is meant to produce and tend a PR — i.e. it is really a dispatch
# worker that belongs in its own session. Matching is case-insensitive.
looks_like_worker() {
	local prompt
	prompt="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
	[[ "$prompt" =~ gh\ pr\ create ]] && return 0
	[[ "$prompt" =~ git\ (push|commit) ]] && return 0
	[[ "$prompt" =~ open\ a\ (pull\ request|pr) ]] && return 0
	[[ "$prompt" =~ self-heal ]] && return 0
	[[ "$prompt" =~ implement\ q[0-9]+ ]] && return 0
	return 1
}

main() {
	# jq is required to parse the hook payload safely.
	command -v jq >/dev/null 2>&1 || emit_allow_unchanged

	local input
	input="$(cat)"
	[[ -n "$input" ]] || emit_allow_unchanged

	local tool_name agent_type isolation prompt
	tool_name="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
	case "$tool_name" in
	Agent | Task) ;;
	*) emit_allow_unchanged ;;
	esac

	agent_type="$(printf '%s' "$input" | jq -r '.tool_input.subagent_type // empty' 2>/dev/null || true)"
	isolation="$(printf '%s' "$input" | jq -r '.tool_input.isolation // empty' 2>/dev/null || true)"
	prompt="$(printf '%s' "$input" | jq -r '.tool_input.prompt // empty' 2>/dev/null || true)"

	# Known read-only agent types are always fine.
	if is_readonly_agent_type "$agent_type"; then
		emit_allow_unchanged
	fi

	# Fire only on the misuse shape: an own-worktree mutation agent, or a prompt
	# that reads like a PR-producing worker.
	if [[ "$isolation" == "worktree" ]] || looks_like_worker "$prompt"; then
		jq -cn '{
			hookSpecificOutput: {
				hookEventName: "PreToolUse",
				permissionDecision: "ask",
				permissionDecisionReason: "This looks like a parallel-dispatch worker (own worktree or PR-producing prompt). Workers must be full Claude Code sessions — spawn a task chip (mcp__ccd_session__spawn_task), not an Agent/Task sub-agent: a sub-agent has no branch of its own, cannot self-heal its PR, and dies with the dispatcher turn. See docs/development/parallel-dispatch.md. Continue only if this is a one-off research/build agent."
			}
		}'
		exit 0
	fi

	emit_allow_unchanged
}

main "$@"
