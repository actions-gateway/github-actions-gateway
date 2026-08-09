#!/usr/bin/env bash
#
# record-launch.sh — run a long command in the background with a stop handle that
# outlives the session's memory of it.
#
# The launching task id is normally the only handle a background run has, and a
# compaction drops it while the process keeps running. What is left is a kill by
# pattern, which is how `pkill -f 'make scripts-test'` reached the `make check`
# running in the same worktree for verification: the gate died mid-run and two
# contaminated results were recorded as genuine red (Q690). See
# docs/development/testing.md#stopping-a-run-name-the-target-never-the-program.
#
# So the handle goes on disk rather than in context: one record per live run
# under tmp/launches/, naming what was launched, which worktree launched it, and
# the exact command that stops it. Reading a record is not recall.
#
# Usage:
#   scripts/agent/record-launch.sh make check > tmp/check.log 2>&1
#   scripts/agent/record-launch.sh --list     # what is running, and how to stop it
#   scripts/agent/record-launch.sh --prune    # drop records whose process is gone
#
# The run gets its own process group, so the recorded stop command reaches its
# children too — a `make` killed alone leaves its whole fan-out behind, which is
# what sends the next person back to a pattern kill. Two consequences:
#
#   - The run must not read stdin. A background process group that reads the
#     terminal takes SIGTTIN and stops. Redirect output to a log and read the
#     log, which is the shape a background run wants anyway.
#   - This wrapper owns the run. Killing the wrapper stops the run, because a
#     surviving process whose record has just been deleted is exactly the state
#     this script exists to prevent.
#
# LAUNCH_RECORD_DIR overrides where records live; tests point it at scratch.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCH_RECORD_DIR="${LAUNCH_RECORD_DIR:-${REPO_ROOT}/tmp/launches}"

RUN_PID=""
RUN_GROUP=no
RECORD_FILE=""

usage() {
	cat <<'EOF'
usage: record-launch.sh <command> [args...]   run it, recorded until it exits
       record-launch.sh --list                what is running, and how to stop it
       record-launch.sh --prune               drop records whose process is gone
EOF
}

# record_field FILE KEY — print KEY's value, empty when absent. Splits on the
# first '=' only, so a recorded command containing one survives the round trip.
record_field() {
	[[ -r "$1" ]] || return 0
	awk -v key="$2" -F= '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$1" 2>/dev/null || true
}

# process_command PID — PID's command line, empty when no such process. A
# function so the test can model a dead, a live, or a recycled pid.
process_command() { ps -o command= -p "$1" 2>/dev/null || true; }

# record_alive PID MARKER — true when PID exists and its command line still
# contains MARKER. `ps` rather than `kill -0`: a recycled pid is a live process
# that is not this run, and killing it is the mistake the whole file is about.
# Both error directions are safe — a false "live" costs a look, never a kill.
record_alive() {
	local pid="$1" marker="$2" cmd
	[[ "${pid}" =~ ^[0-9]+$ ]] || return 1
	cmd="$(process_command "${pid}")"
	[[ -n "${cmd}" ]] || return 1
	[[ -z "${marker}" || "${cmd}" == *"${marker}"* ]]
}

# record_state FILE — live when the recorded process is still there, stale when
# it is gone (a run killed with SIGKILL leaves its record behind).
record_state() {
	if record_alive "$(record_field "$1" pid)" "$(record_field "$1" marker)"; then
		echo live
	else
		echo stale
	fi
}

list_records() {
	local file found=0 state
	for file in "${LAUNCH_RECORD_DIR}"/*.launch; do
		[[ -e "${file}" ]] || continue
		found=1
		state="$(record_state "${file}")"
		printf '%-5s pid %-8s %s\n' "${state}" \
			"$(record_field "${file}" pid)" "$(record_field "${file}" command)"
		printf '      worktree %s\n' "$(record_field "${file}" worktree)"
		printf '      stop     %s\n' "$(record_field "${file}" stop)"
		if [[ "$(record_field "${file}" group)" != yes ]]; then
			printf '      note     not a process-group leader; its children may survive\n'
		fi
	done
	if ((found == 0)); then
		echo "no launch records in ${LAUNCH_RECORD_DIR}"
	fi
}

prune_records() {
	local file pruned=0
	for file in "${LAUNCH_RECORD_DIR}"/*.launch; do
		[[ -e "${file}" ]] || continue
		if [[ "$(record_state "${file}")" == live ]]; then
			continue
		fi
		rm -f "${file}"
		pruned=$((pruned + 1))
	done
	echo "record-launch: pruned ${pruned} stale record(s)"
}

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT INT TERM`.
cleanup() {
	if [[ -n "${RUN_PID}" ]] && kill -0 "${RUN_PID}" 2>/dev/null; then
		if [[ "${RUN_GROUP}" == yes ]]; then
			kill -TERM -- "-${RUN_PID}" 2>/dev/null || true
		else
			kill -TERM "${RUN_PID}" 2>/dev/null || true
		fi
	fi
	if [[ -n "${RECORD_FILE}" ]]; then
		rm -f "${RECORD_FILE}"
	fi
}

launch() {
	mkdir -p "${LAUNCH_RECORD_DIR}"

	# Job control, so the run is its own process group. Turned back off straight
	# away to keep bash's job-status chatter out of the run's own output.
	set -m
	"$@" &
	RUN_PID=$!
	set +m
	trap cleanup EXIT INT TERM

	# Measured, not assumed: if job control did not give the run its own group,
	# a group kill would take this shell and everything else in its group with
	# it, so the recorded stop command names the process alone instead. The
	# `|| pgid=""` is load-bearing under `pipefail` — a run short enough to have
	# already exited makes `ps` fail, and an unguarded assignment would end the
	# wrapper here, reporting its own status as the run's.
	local pgid stop
	pgid="$(ps -o pgid= -p "${RUN_PID}" 2>/dev/null | tr -d ' ')" || pgid=""
	if [[ "${pgid}" == "${RUN_PID}" ]]; then
		RUN_GROUP=yes
		stop="kill -TERM -- -${RUN_PID}"
	else
		stop="kill -TERM ${RUN_PID}"
	fi

	local now
	now="$(date +%s)"
	RECORD_FILE="${LAUNCH_RECORD_DIR}/${now}-${RUN_PID}.launch"
	{
		printf 'pid=%s\n' "${RUN_PID}"
		printf 'group=%s\n' "${RUN_GROUP}"
		printf 'host=%s\n' "${HOSTNAME:-$(hostname)}"
		printf 'started=%s\n' "${now}"
		printf 'worktree=%s\n' "${REPO_ROOT}"
		printf 'command=%s\n' "$*"
		printf 'marker=%s\n' "$(basename -- "$1")"
		printf 'stop=%s\n' "${stop}"
	} >"${RECORD_FILE}"

	echo "record-launch: ${RECORD_FILE}" >&2
	echo "record-launch: stop with: ${stop}" >&2

	local rc=0
	wait "${RUN_PID}" || rc=$?
	RUN_PID=""
	return "${rc}"
}

main() {
	case "${1:-}" in
	--list) list_records ;;
	--prune) prune_records ;;
	-h | --help) usage ;;
	"")
		usage >&2
		return 2
		;;
	*) launch "$@" ;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
