#!/usr/bin/env bash
#
# Behavioural tests for scripts/agent/record-launch.sh — the on-disk stop handle
# for a background run (Q709).
#
# Why it is tested: the record's whole value is that somebody who has lost the
# launching handle can act on it without guessing, so a wrong field is a kill
# aimed at the wrong process — the failure the script exists to prevent, now
# wearing a record's authority. Two claims carry that weight and neither is
# inferable from reading the source:
#
#   - the run really is its own process-group leader, so the recorded group kill
#     cannot reach this shell; and
#   - the recorded stop command really does take the run's children with it,
#     which is the reason it is a group kill rather than a plain one.
#
# Both are asserted against real processes, by running the recorded command
# verbatim and then confirming a grandchild is gone. The pid-liveness rules are
# asserted against a stubbed `ps`, so a pid is whatever the test says it is.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${REPO_ROOT}/scripts/agent/record-launch.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Set before sourcing: the default resolves under the worktree at source time,
# and no test may write there.
LAUNCH_RECORD_DIR="${WORKDIR}/launches"
export LAUNCH_RECORD_DIR
# shellcheck source=scripts/agent/record-launch.sh
source "${SCRIPT}"

fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

# --- Liveness, against a stubbed ps ------------------------------------------

# LIVE_PIDS maps a pid to the command line `ps` would report. A pid that is not
# a key is a dead process — empty output, which is what `ps -o command=` gives.
declare -A LIVE_PIDS=()
process_command() { echo "${LIVE_PIDS[$1]:-}"; }

write_record() {
	mkdir -p "${LAUNCH_RECORD_DIR}"
	local file="${LAUNCH_RECORD_DIR}/$1.launch"
	shift
	printf '%s\n' "$@" >"${file}"
	echo "${file}"
}

LIVE_PIDS=([4242]="make check")
rec="$(write_record 100-4242 pid=4242 marker=make group=yes 'command=make check')"
check "a live process with its marker reads live" live "$(record_state "${rec}")"

LIVE_PIDS=()
check "a process that is gone reads stale" stale "$(record_state "${rec}")"

# A recycled pid is the case bare liveness gets wrong: the process is real, and
# it is not the run. Killing it is exactly the mistake being avoided.
LIVE_PIDS=([4242]="/usr/sbin/cupsd -l")
check "a recycled pid reads stale" stale "$(record_state "${rec}")"

rec_bad="$(write_record 100-none pid=not-a-pid marker=make)"
check "a malformed pid reads stale" stale "$(record_state "${rec_bad}")"

# The recorded command is the thing a reader identifies the run by, and it very
# often contains an '=' (`make check TIMEOUT=900`).
rec_eq="$(write_record 100-7 pid=7 'command=make check TIMEOUT=900')"
check "a recorded value may contain the separator" "make check TIMEOUT=900" \
	"$(record_field "${rec_eq}" command)"
check "an absent key reads empty" "" "$(record_field "${rec_eq}" nope)"

rm -rf "${LAUNCH_RECORD_DIR}"

# --- Real processes ----------------------------------------------------------

# A run with a child of its own: `make` fans out, and a stop that reaches only
# the process named in the record would leave the fan-out running.
cat >"${WORKDIR}/tree.sh" <<'EOF'
#!/usr/bin/env bash
sleep 300 &
echo $! >"$1"
wait
EOF

pid_file="${WORKDIR}/grandchild.pid"
"${SCRIPT}" bash "${WORKDIR}/tree.sh" "${pid_file}" >"${WORKDIR}/launch.log" 2>&1 &
wrapper_pid=$!

# Bounded, so the suite ends on its own if the launch never comes up.
record=""
for _ in $(seq 1 100); do
	if [[ -s "${pid_file}" ]]; then
		record="$(find "${LAUNCH_RECORD_DIR}" -name '*.launch' 2>/dev/null | head -1)"
		[[ -n "${record}" ]] && break
	fi
	sleep 0.1
done

if [[ -z "${record}" ]]; then
	echo "FAIL the launch wrote no record within 10s" >&2
	kill "${wrapper_pid}" 2>/dev/null || true
	exit 1
fi
echo "ok   a live run has a record"

run_pid="$(record_field "${record}" pid)"
grandchild="$(cat "${pid_file}")"

check "the record names this worktree" "${REPO_ROOT}" "$(record_field "${record}" worktree)"
check "the record names what was launched" "bash" "$(record_field "${record}" marker)"

# The load-bearing measurement: the run leads its own process group, so the
# recorded group kill cannot reach this shell.
check "the run is its own process-group leader" "${run_pid}" \
	"$(ps -o pgid= -p "${run_pid}" | tr -d ' ')"
check "and the record says so" yes "$(record_field "${record}" group)"
check "so the stop command is a group kill" "kill -TERM -- -${run_pid}" \
	"$(record_field "${record}" stop)"

# Run the recorded command verbatim — a stop handle that needs editing is not a
# handle. The grandchild is what proves the group form was necessary.
eval "$(record_field "${record}" stop)"

stopped=no
for _ in $(seq 1 100); do
	if ! kill -0 "${grandchild}" 2>/dev/null && ! kill -0 "${run_pid}" 2>/dev/null; then
		stopped=yes
		break
	fi
	sleep 0.1
done
check "the recorded stop takes the run AND its children" yes "${stopped}"

wait "${wrapper_pid}" 2>/dev/null || true
check "the record is gone once the run ends" "" \
	"$(find "${LAUNCH_RECORD_DIR}" -name '*.launch' 2>/dev/null)"

# --- Exit status and record hygiene ------------------------------------------

# A distinctive status, because 1 is also what the wrapper exits with when it
# dies on its own `set -e` — which is what it did until `ps` failing on an
# already-exited run was guarded. A `false` here would have read as a pass.
rc=0
bash -c '"$0" bash -c "exit 7"' "${SCRIPT}" >/dev/null 2>&1 || rc=$?
check "the wrapper returns the command's status" 7 "${rc}"

rc=0
"${SCRIPT}" true >/dev/null 2>&1 || rc=$?
check "a run too short to outlive its own launch still reports success" 0 "${rc}"
check "a completed run leaves no record" "" \
	"$(find "${LAUNCH_RECORD_DIR}" -name '*.launch' 2>/dev/null)"

# --- --list and --prune ------------------------------------------------------

rm -rf "${LAUNCH_RECORD_DIR}"
check "--list says so when there is nothing to stop" \
	"no launch records in ${LAUNCH_RECORD_DIR}" "$("${SCRIPT}" --list)"

# Real pids this time, so --prune is exercised through the real `ps`: this
# shell is live, and a pid past the system maximum can never be.
write_record 100-live "pid=$$" marker=bash group=yes 'command=self' >/dev/null
write_record 100-dead pid=4194304 marker=make group=yes 'command=make check' >/dev/null
"${SCRIPT}" --prune >/dev/null
check "--prune drops the record of a process that is gone" "" \
	"$(find "${LAUNCH_RECORD_DIR}" -name '100-dead.launch')"
check "--prune keeps a record whose process is live" \
	"${LAUNCH_RECORD_DIR}/100-live.launch" \
	"$(find "${LAUNCH_RECORD_DIR}" -name '100-live.launch')"

if ((fails > 0)); then
	echo "record-launch-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "record-launch-test: ok"
