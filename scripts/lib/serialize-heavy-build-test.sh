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

# gone PID — true once PID is dead: absent, or a zombie, since an orphan waits on
# whatever reaps for PID 1 and some containers' PID 1 never does. A signalled
# orphan dies on its own schedule, so this waits, up to 60 s; a pass returns as
# soon as it dies, and only a failure spends the bound. Work that outlived the
# lock would run on for the rest of its 300 s.
gone() {
	local i stat
	for ((i = 0; i < 600; i++)); do
		stat="$(ps -o stat= -p "$1" 2>/dev/null)" || return 0
		stat="${stat// /}"
		[[ -z "${stat}" || "${stat}" == Z* ]] && return 0
		sleep 0.1
	done
	return 1
}

LAUNCHER=""

# start RUN SECONDS [RC] — launch work.sh in its own process group; sets LAUNCHER,
# whose pid is the perl lock holder once work.sh has re-exec'd. The relayed
# signals start at their default: run-parallel.sh backgrounds this suite, which
# leaves INT ignored, and bash cannot un-ignore a signal it inherited ignored.
start() {
	set -m
	perl -e '$SIG{$_} = "DEFAULT" for qw(HUP INT QUIT TERM); exec @ARGV or exit 127' \
		"${WORKDIR}/root/work.sh" "$@" &
	LAUNCHER=$!
	set +m
}

# signalled NAME KILL_ARGS WANT_RC — start a run, signal it once it is inside
# the section, and assert the launcher's status and that no work outlived it.
signalled() {
	local name="$1" target="$2" sig="$3" want="$4"
	local run="${name// /-}"
	start "${run}" 300
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
	# The child is the launcher's to reap, so it is gone before the launcher
	# exits: an ordering, checked with no wait. The grandchild is not.
	# Survivors are killed on the failure branches only: after a pass both pids
	# are dead, and signalling them could reach a process that reused one.
	if ((rc != want)); then
		bad "${name}: launcher exited ${rc}, want ${want}"
		kill -KILL "${child}" "${grandchild}" 2>/dev/null || true
	elif kill -0 "${child}" 2>/dev/null; then
		bad "${name}: child ${child} outlived the lock"
		kill -KILL "${child}" "${grandchild}" 2>/dev/null || true
	elif ! gone "${grandchild}"; then
		bad "${name}: grandchild ${grandchild} outlived the lock"
		kill -KILL "${grandchild}" 2>/dev/null || true
	else
		ok "${name}"
	fi
}

# ignored NAME SIG — launch with SIG already ignored, as nohup does for HUP and a
# non-interactive `cmd &` for INT, and assert SIG sent to the launcher alone is
# still ignored: the work runs to completion and the launcher exits 0.
ignored() {
	local name="$1" sig="$2"
	local run="${name// /-}"
	set -m
	(
		trap '' "${sig}"
		exec "${WORKDIR}/root/work.sh" "${run}" 5
	) &
	LAUNCHER=$!
	set +m
	if ! await "${WORKDIR}/${run}.grandchild"; then
		bad "${name}: work never entered the section"
		kill -KILL -- "-${LAUNCHER}" 2>/dev/null || true
		return
	fi
	# Otherwise a late await sees work that already finished, and passes untested.
	if [[ -e "${WORKDIR}/${run}.exit" ]]; then
		bad "${name}: work finished before the signal was sent"
		wait "${LAUNCHER}" || true
		return
	fi
	kill "-${sig}" "${LAUNCHER}"
	local rc=0
	wait "${LAUNCHER}" || rc=$?
	if ((rc != 0)); then
		bad "${name}: launcher exited ${rc}, want 0"
		kill -KILL -- "-${LAUNCHER}" 2>/dev/null || true
	elif [[ ! -s "${WORKDIR}/${run}.exit" ]]; then
		bad "${name}: work exited 0 without finishing"
	else
		ok "${name}"
	fi
}

signalled "TERM to the launcher alone" pid TERM 143
signalled "HUP to the launcher alone" pid HUP 129
signalled "TERM to the group" group TERM 143
signalled "INT to the group" group INT 130
ignored "HUP ignored on entry stays ignored" HUP
ignored "INT ignored on entry stays ignored" INT

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
