#!/usr/bin/env bash
#
# Phase event stream for the release-validation gate, and the status renderer
# over it (Q615, Q616).
#
# The gate is an hour-long walk-away command with a phase per leg. This records
# each transition as one JSON line so a renderer can answer "where is it" at any
# moment without replaying the terminal. Same shape and the same reasons as the
# e2e suite's spec stream (docs/development/testing.md § Watching an e2e run in
# progress): the stream is the contract, renderers read it, and nothing parses
# human-readable output to derive state.
#
# Two renderers consume it. The human-facing one is the gate's own terminal
# output. The agent-facing one is progress_status_json below, reduced to a
# single object the gate rewrites into RELEASE_STATUS_FILE after every event —
# one read answers "where is it" without replaying the stream. release-sentinel.sh
# renders the same object to decide when to wake a session.
#
# Best-effort by design: a progress-reporting failure must never fail a release
# gate, so every error path is silent.

# The repo root, derived from this file's own location so the defaults below
# resolve the same whether the gate, the sentinel, or a test sourced it.
PROGRESS_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# RELEASE_PROGRESS_FILE — where the stream lands. Repo-local tmp/ (gitignored),
# never the host temp dir: concurrent worktrees collide there, and it sits
# outside the workspace where sandboxed tooling cannot read it back. Set empty
# to disable the stream; the human-facing echo still happens.
RELEASE_PROGRESS_FILE="${RELEASE_PROGRESS_FILE-${PROGRESS_REPO_ROOT}/tmp/release-validation-progress.jsonl}"

# RELEASE_STATUS_FILE — the rendered status object. Derived from the stream and
# rewritten atomically after every event, so a reader never sees a half-written
# object. Defaults beside the stream rather than to a fixed path, so a suite
# that points RELEASE_PROGRESS_FILE at its own scratch dir scopes this too, and
# an empty stream leaves no live path reachable at all (Q786). Set empty to
# disable this file alone; the stream is unaffected.
RELEASE_STATUS_FILE="${RELEASE_STATUS_FILE-${RELEASE_PROGRESS_FILE:+$(dirname "${RELEASE_PROGRESS_FILE}")/release-validation-status.json}}"

# Heartbeat text relayed into the status object is capped here. It originates in
# a GitHub Actions job log, so it is data an agent reads, not a line this repo
# controls end to end — a cap keeps one pathological line from dominating a
# report.
RELEASE_HEARTBEAT_MAX_CHARS="${RELEASE_HEARTBEAT_MAX_CHARS:-200}"

# progress_init — start a fresh stream and status for this run. Called before
# the gate's preflight, so no reader can meet the previous run's terminal event
# during the minutes a preflight can take.
progress_init() {
	# No stream, nothing to initialize — including the status file, which a
	# disabled stream can no longer refresh either (progress_status_write).
	# Removing it here regardless would reach a path the caller opted out of.
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	mkdir -p "$(dirname "${RELEASE_PROGRESS_FILE}")" 2>/dev/null || return 0
	: >"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
	# A status file left by the previous run would otherwise read as this one's
	# until the first event lands.
	[[ -n "${RELEASE_STATUS_FILE}" ]] && rm -f "${RELEASE_STATUS_FILE}" 2>/dev/null
	progress_status_write
}

# progress_event PHASE STATE [DETAIL] — append one event. STATE is start|done|fail.
# On a `gate start` event DETAIL is the RC tag: progress_status_json surfaces it
# as .rc, which is the only structural meaning any detail carries.
progress_event() {
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	local phase="$1" state="$2" detail="${3:-}"
	# jq builds the line so a detail containing a quote or a newline cannot
	# produce a malformed record.
	jq -cn --arg phase "$phase" --arg state "$state" --arg detail "$detail" \
		--argjson t "$(date +%s)" \
		'{kind:"phase", t:$t, phase:$phase, state:$state} + (if $detail == "" then {} else {detail:$detail} end)' \
		>>"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
	progress_status_write
}

# progress_heartbeat TEXT — record the latest relayed e2e spec heartbeat. The
# e2e leg is ~25 minutes of one phase, so without this the status object would
# say "e2e, started 18 minutes ago" and nothing else; the heartbeat is what
# makes progress inside the phase visible. Not echoed — the relay that produces
# the line has already printed it.
progress_heartbeat() {
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	# Only progress_init creates the stream. The relay that calls this also runs
	# standalone as a recovery path, and a heartbeat that could conjure a stream
	# would leave a renderer looking at a gate that is not running.
	[[ -f "${RELEASE_PROGRESS_FILE}" ]] || return 0
	local text="${1:0:${RELEASE_HEARTBEAT_MAX_CHARS}}"
	jq -cn --arg text "$text" --argjson t "$(date +%s)" \
		'{kind:"heartbeat", t:$t, text:$text}' \
		>>"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
	progress_status_write
}

# progress_run REPO RUN_ID — record the GitHub run the gate is now parked on.
# The status object surfaces it as .runRepo/.runId so a renderer can ask GitHub
# whether that run is still live (Q630). The heartbeat above cannot answer that:
# it needs a fetchable job log, and a log GitHub will not serve is
# indistinguishable from a run that has stopped moving.
progress_run() {
	[[ -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	# Same reason as progress_heartbeat: the relay also runs standalone, and a
	# run reference must not conjure a stream for a gate that is not running.
	[[ -f "${RELEASE_PROGRESS_FILE}" ]] || return 0
	jq -cn --arg repo "$1" --arg id "$2" --argjson t "$(date +%s)" \
		'{kind:"run", t:$t, repo:$repo, id:$id}' \
		>>"${RELEASE_PROGRESS_FILE}" 2>/dev/null || true
	progress_status_write
}

# progress_phase PHASE MESSAGE — announce a phase to the operator and record its
# start. The echo is what a human reads; the event is what a renderer reads.
progress_phase() {
	local phase="$1" message="$2"
	printf '\n==> [%s] %s\n' "$phase" "$message"
	progress_event "$phase" start "$message"
}

# progress_status_json [STREAM_FILE] — reduce the stream to one status object on
# stdout. Pure: reads the stream, writes nothing. An absent or empty stream is
# not an error — it renders gate="preflight", which is the true state of a gate
# that has not committed to running yet. The gate empties the stream before its
# preflight and writes its first event after it, so both an aborted preflight
# and an in-progress one leave an empty stream, and neither can be mistaken for
# an in-flight gate or for the previous run's verdict.
#
# Malformed lines are skipped rather than fatal: the gate appends while a reader
# may be mid-read, and a torn final line must cost a field, not the whole object.
#
# RELEASE_STATUS_NOW pins "now" for the elapsed fields (tests).
progress_status_json() {
	local stream="${1:-${RELEASE_PROGRESS_FILE}}"
	local now="${RELEASE_STATUS_NOW:-$(date +%s)}"
	# A stream that does not exist yet reads exactly like an empty one.
	[[ -n "${stream}" && -r "${stream}" ]] || stream=/dev/null
	jq -Rn --argjson now "$now" '
		[inputs | (fromjson? // empty)] as $events
		| ($events | map(select(.kind == "phase"))) as $phases
		| ($events | map(select(.kind == "heartbeat")) | last) as $beat
		| ($events | map(select(.kind == "run")) | last) as $run
		| ($phases | last) as $cur
		| ($phases | map(select(.phase == "gate" and .state == "start")) | last) as $started
		# The FIRST failure, not the last: the gate reports its own exit as a
		# second fail event from the teardown trap, and the phase that actually
		# broke is the diagnosis.
		| ($phases | map(select(.state == "fail")) | first) as $failed
		| {
			gate: (
				if ($phases | length) == 0 then "preflight"
				elif $failed then "failed"
				elif ($phases | any(.phase == "gate" and .state == "done")) then "passed"
				else "running" end),
			rc: ($started.detail // null),
			phase: ($cur.phase // null),
			state: ($cur.state // null),
			detail: ($cur.detail // null),
			startedAt: ($started.t // null),
			updatedAt: (($events | last).t // null),
			elapsed: (if $started then $now - $started.t else null end),
			phaseElapsed: (if $cur then $now - $cur.t else null end),
			idle: (if ($events | length) > 0 then $now - ($events | last).t else null end),
			heartbeat: ($beat.text // null),
			heartbeatAge: (if $beat then $now - $beat.t else null end),
			runRepo: ($run.repo // null),
			runId: ($run.id // null),
			failure: (if $failed then "\($failed.phase): \($failed.detail // "failed")" else null end)
		}' <"$stream" 2>/dev/null || echo 'null'
}

# progress_status_write — refresh RELEASE_STATUS_FILE from the stream. Writes a
# temp file in the same directory and renames it, so a reader polling the file
# sees either the previous object or the new one, never a partial write. A
# rendering failure leaves the last good object in place.
progress_status_write() {
	# No stream, no status: disabling the stream disables both renderers rather
	# than leaving a status file frozen at "preflight" for the whole run.
	[[ -n "${RELEASE_STATUS_FILE}" && -n "${RELEASE_PROGRESS_FILE}" ]] || return 0
	mkdir -p "$(dirname "${RELEASE_STATUS_FILE}")" 2>/dev/null || return 0
	local tmp="${RELEASE_STATUS_FILE}.$$"
	if progress_status_json >"${tmp}" 2>/dev/null && [[ -s "${tmp}" ]]; then
		mv -f "${tmp}" "${RELEASE_STATUS_FILE}" 2>/dev/null || rm -f "${tmp}" 2>/dev/null
	else
		rm -f "${tmp}" 2>/dev/null
	fi
	return 0
}
