#!/usr/bin/env bash
#
# Phase event stream for the release-validation gate (Q615).
#
# The gate is an hour-long walk-away command with eight phases. This records
# each transition as one JSON line so a renderer can answer "where is it" at any
# moment without replaying the terminal. Same shape and the same reasons as the
# e2e suite's spec stream (docs/development/testing.md § Watching an e2e run in
# progress): the stream is the contract, renderers read it, and nothing parses
# human-readable output to derive state.
#
# Phase 2 (Q616) adds the two renderers this exists for — a status file and a
# sentinel that wakes an agent on transitions. Phase 1 writes the stream and
# prints the human-facing line.
#
# Best-effort by design: a progress-reporting failure must never fail a release
# gate, so every error path is silent.

# RELEASE_PROGRESS_FILE — where the stream lands. Repo-local tmp/ (gitignored),
# never the host temp dir: concurrent worktrees collide there, and it sits
# outside the workspace where sandboxed tooling cannot read it back. Set empty
# to disable the stream; the human-facing echo still happens.
RELEASE_PROGRESS_FILE="${RELEASE_PROGRESS_FILE:-}"

# progress_init — start a fresh stream for this run.
progress_init() {
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	mkdir -p "$(dirname "${RELEASE_PROGRESS_FILE}")" 2>/dev/null || return 0
	: >"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
}

# progress_event PHASE STATE [DETAIL] — append one event. STATE is start|done|fail.
progress_event() {
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	local phase="$1" state="$2" detail="${3:-}"
	# jq builds the line so a detail containing a quote or a newline cannot
	# produce a malformed record.
	jq -cn --arg phase "$phase" --arg state "$state" --arg detail "$detail" \
		--argjson t "$(date +%s)" \
		'{kind:"phase", t:$t, phase:$phase, state:$state} + (if $detail == "" then {} else {detail:$detail} end)' \
		>>"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
}

# progress_phase PHASE MESSAGE — announce a phase to the operator and record its
# start. The echo is what a human reads; the event is what a renderer reads.
progress_phase() {
	local phase="$1" message="$2"
	printf '\n==> [%s] %s\n' "$phase" "$message"
	progress_event "$phase" start "$message"
}
