#!/usr/bin/env bash
#
# Behavioural tests for serialize_heavy_build in scripts/lib/common.sh, and the
# RunRelayed.pm signal relay it shares with deploy/monitoring/preview/render.sh.
#
# The lock lives in a perl launcher and the work in its child, so a signal that
# kills the launcher alone frees the lock while the work runs on (Q1093). The
# single-pid cases assert the work is gone by the time the launcher has exited:
# that is the only order in which the next holder cannot enter beside it. The
# group cases pin the paths that already worked (Ctrl-C, record-launch.sh's
# group stop), so the relay cannot trade a rare orphan for a routine one.
#
# Each run's work is a bash script running a grandchild in the foreground, the
# shape of a real gate: bash killed alone leaves its running command behind,
# so relaying to the child alone would pass a child-only check.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# A stand-in repo root: a one-slot throttle with its lock in WORKDIR, and the
# real scripts/lib so the launcher loads the RunRelayed.pm under test.
mkdir -p "${WORKDIR}/root/scripts/agent"
ln -s "${REPO_ROOT}/scripts/lib" "${WORKDIR}/root/scripts/lib"
cat >"${WORKDIR}/root/scripts/agent/local-throttle.sh" <<'EOF'
#!/usr/bin/env bash
case "$1" in
slots) echo 1 ;;
lockfile) echo "${WORKDIR}/slot.lock" ;;
esac
EOF
# work.sh RUN SECONDS [RC] — take the lock, record entry and pids under
# WORKDIR/RUN.*, run a grandchild for SECONDS, record exit, exit RC.
cat >"${WORKDIR}/root/work.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${REPO_ROOT}/scripts/lib/common.sh"
serialize_heavy_build "$@"
run="${WORKDIR}/$1"
echo "$$" >"${run}.child"
date +%s >"${run}.enter"
sh -c 'echo "$$" >"$1"; exec sleep "$2"' _ "${run}.grandchild" "$2"
date +%s >"${run}.exit"
exit "${3:-0}"
EOF
chmod +x "${WORKDIR}/root/scripts/agent/local-throttle.sh" "${WORKDIR}/root/work.sh"
export WORKDIR
unset GAG_HEAVY_BUILD_LOCK_HELD

fails=0
ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1" >&2
	fails=$((fails + 1))
}

# await FILE — wait up to 60 s for FILE to exist. The bound only limits how long
# a failure takes to report, so it is sized for a host at several times its core
# count (the scripts-test fan-out), not for an idle one.
await() {
	local i
	for ((i = 0; i < 600; i++)); do
		[[ -s "$1" ]] && return 0
		sleep 0.1
	done
	return 1
}

# gone PID — true once PID is dead, allowing 10 s for an orphan to be reaped;
# work that outlived the lock runs on for the rest of its 120 s.
gone() {
	local i
	for ((i = 0; i < 100; i++)); do
		kill -0 "$1" 2>/dev/null || return 0
		sleep 0.1
	done
	return 1
}

LAUNCHER=""

# start RUN SECONDS [RC] — launch work.sh in its own process group; sets LAUNCHER,
# whose pid is the perl lock holder once work.sh has re-exec'd.
start() {
	set -m
	"${WORKDIR}/root/work.sh" "$@" &
	LAUNCHER=$!
	set +m
}

# signalled NAME KILL_ARGS WANT_RC — start a run, signal it once it is inside
# the section, and assert the launcher's status and that no work outlived it.
signalled() {
	local name="$1" target="$2" sig="$3" want="$4"
	local run="${name// /-}"
	start "${run}" 120
	if ! await "${WORKDIR}/${run}.grandchild"; then
		bad "${name}: work never entered the section"
		kill -KILL -- "-${LAUNCHER}" 2>/dev/null || true
		return
	fi
	if [[ "${target}" == group ]]; then
		kill "-${sig}" -- "-${LAUNCHER}"
	else
		kill "-${sig}" "${LAUNCHER}"
	fi
	local rc=0
	wait "${LAUNCHER}" || rc=$?
	local child grandchild
	child="$(<"${WORKDIR}/${run}.child")"
	grandchild="$(<"${WORKDIR}/${run}.grandchild")"
	if ((rc != want)); then
		bad "${name}: launcher exited ${rc}, want ${want}"
	elif ! gone "${child}" || ! gone "${grandchild}"; then
		bad "${name}: work outlived the lock (child ${child}, grandchild ${grandchild})"
	else
		ok "${name}"
	fi
	kill -KILL "${child}" "${grandchild}" 2>/dev/null || true
}

signalled "TERM to the launcher alone" pid TERM 143
signalled "HUP to the launcher alone" pid HUP 129
signalled "TERM to the group" group TERM 143
signalled "INT to the group" group INT 130

# The child's own status still comes back through the relay.
start status 0 3
rc=0
wait "${LAUNCHER}" || rc=$?
if ((rc == 3)); then ok "exit status passes through"; else bad "exit status: got ${rc}, want 3"; fi

# And the lock still excludes: B queues until A has left the section.
start a 2
await "${WORKDIR}/a.enter" || bad "A never entered"
a_pid="${LAUNCHER}"
start b 0
b_pid="${LAUNCHER}"
wait "${a_pid}" "${b_pid}"
if (($(<"${WORKDIR}/b.enter") >= $(<"${WORKDIR}/a.exit"))); then
	ok "a second run queues behind the first"
else
	bad "B entered at $(<"${WORKDIR}/b.enter"), before A left at $(<"${WORKDIR}/a.exit")"
fi

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all serialize_heavy_build tests passed"
