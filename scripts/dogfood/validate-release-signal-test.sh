#!/usr/bin/env bash
#
# Signal tests for scripts/dogfood/validate-release.sh (Q1392): every way a
# signal can end the gate records a failure with that signal's status, dumps the
# diagnostics, and runs both stop scripts.
#
# Bash runs an EXIT trap after an untrapped signal with the last command's
# status, 0, so before Q1392 a Ctrl-C, a `kill` or a closed terminal reached
# teardown as a pass: no failure event, no diagnostics, and a stream still
# reading `running`. A closed terminal also failed teardown's first echo, and
# errexit ended the trap there with the nodes still billing.
#
# Each case runs a real gate process: validate-release.sh sourced with its
# teardown armed by arm_teardown, as main() arms it, around one step that
# blocks. Cluster calls and the stop scripts are stubbed; the stubs print before
# they log, as the real stop scripts print before they act, so an output that
# is gone fails them the way it fails the real ones. python3 starts each gate in
# a session of its own with INT, TERM and HUP at their defaults, delivers the
# ending, and reports the gate's exit status. A closed terminal is a pty whose
# session leader stands in for the shell, resending the hangup to its job as
# bash does: on macOS closing the master HUPs the session leader alone. Nothing
# touches a cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

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

mkdir -p "${WORK}/stubs"
for stop in e2e-stop stop; do
	cat >"${WORK}/stubs/${stop}.sh" <<STUB
set -euo pipefail
echo "stopping (${stop} stub)"
echo "${stop}" >>"\${WORK}/events"
# The second-signal case: hold here until the harness has sent it.
if [[ -n "\${SECOND:-}" && "${stop}" == e2e-stop ]]; then
	: >"\${WORK}/second"
	for _ in \$(seq 50); do [[ ! -e "\${WORK}/second" ]] && break; sleep 0.1; done
fi
STUB
done

# The gate. SHAPE picks the step the ending interrupts.
cat >"${WORK}/gate.sh" <<'GATE'
set -euo pipefail
shopt -s inherit_errexit
source "${REPO_ROOT}/scripts/dogfood/validate-release.sh"
source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"
PROJECT=p ZONE=z CLUSTER=c REPO=octo/repo SCRIPT_DIR="${WORK}/stubs" WORKDIR="${WORK}/workdir"
mkdir -p "${WORKDIR}"
# The diagnostics dump is the caller that records; the readings leg pins too.
gke_get_credentials_and_verify() {
	[[ "${FUNCNAME[1]}" == dump_diagnostics ]] || return 0
	echo diagnostics >>"${WORK}/events"
	return 1
}
ready() { : >"${WORK}/ready"; }
block() {
	ready
	sleep "${BLOCK_SECONDS:-30}"
}
if [[ "${SHAPE}" == lease-lost ]]; then
	lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" v0.0.0-rc.0
fi
arm_teardown
if [[ "${SHAPE}" == lease-lost ]]; then
	lease_renew_start "${PROJECT}" "${ZONE}" "${CLUSTER}" stop-owner
fi
progress_event gate start v0.0.0-rc.0
progress_phase e2e "the step the ending interrupts"
case "${SHAPE}" in
fail)
	ready
	false
	;;
block | lease-lost) block ;;
subst) echo "$(block)" ;;
wait)
	# e2e_leg itself, down to its `wait "${capture_pid}" || true`.
	pool_node_count() { echo 0; }
	dispatch_e2e_run() { E2E_RESOLVED_RUN_ID=1; }
	report_e2e_run() { :; }
	census_mirror_clients() { :; }
	capture_worker_sizing() {
		ready
		sleep 5
	}
	printf 'exit 0\n' >"${SCRIPT_DIR}/e2e-start.sh"
	printf 'exit 0\n' >"${SCRIPT_DIR}/e2e-run-watch.sh"
	e2e_leg
	;;
readings)
	E2E_DRAGONFLY_NODES=0
	for r in battery p2p worker connect; do eval "dragonfly_${r}_reading() { :; }"; done
	dragonfly_readings
	block
	;;
esac
progress_event gate done "legs PASSED for v0.0.0-rc.0"
GATE

cat >"${WORK}/ending.py" <<'PY'
"""Start the gate, deliver ENDING once it blocks, print its exit status.

ENDING is SIG-group or SIG-gate (a signal to the gate's process group, or to
its pid alone), SIG-group-twice (a second one once teardown is under way),
tty-closed (close the gate's controlling terminal, output to that terminal),
tty-closed-file (the same, output to a file), lease-lost, or none.
"""
import os, pty, select, signal, sys, time

ending, gate, work = sys.argv[1:4]
for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(s, signal.SIG_DFL)
log = os.path.join(work, "gate.out")


def exists(name):
    return os.path.exists(os.path.join(work, name))


def await_file(name, fd=None, seconds=60):
    deadline = time.time() + seconds
    while time.time() < deadline and not exists(name):
        if fd is None:
            time.sleep(0.05)
            continue
        if select.select([fd], [], [], 0.05)[0]:
            try:
                os.read(fd, 65536)
            except OSError:
                pass
    return exists(name)


def to_log():
    out = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_APPEND)
    os.dup2(out, 1)
    os.dup2(out, 2)


def shell():
    """An interactive shell's part in a hangup: own the terminal, run the gate
    as its foreground job, and resend SIGHUP to that job, as bash does. Closing
    the master HUPs only the session leader on macOS."""
    signal.signal(signal.SIGTTOU, signal.SIG_IGN)
    job = os.fork()
    if job == 0:
        os.setpgid(0, 0)
        signal.signal(signal.SIGTTOU, signal.SIG_DFL)
        if ending == "tty-closed-file":
            to_log()
        os.execvp("bash", ["bash", gate])
    os.setpgid(job, job)
    with open(os.path.join(work, "job-pid"), "w") as f:
        f.write(str(job))
    os.tcsetpgrp(0, job)
    signal.signal(signal.SIGHUP, lambda *_: os.killpg(job, signal.SIGHUP))
    status = os.waitpid(job, 0)[1]
    with open(os.path.join(work, "job-status"), "w") as f:
        f.write(str(os.waitstatus_to_exitcode(status)))
    os._exit(0)


if ending.startswith("tty"):
    pid, master = pty.fork()
    if pid == 0:
        shell()
    await_file("ready", master)
    time.sleep(0.3)
    os.close(master)
else:
    pid = os.fork()
    if pid == 0:
        os.setsid()
        os.dup2(os.open(os.devnull, os.O_RDONLY), 0)
        to_log()
        os.execvp("bash", ["bash", gate])
    await_file("ready")
    time.sleep(0.3)
    if ending not in ("none", "lease-lost"):
        name, target = ending.split("-", 1)
        sig = getattr(signal, "SIG" + name)
        (os.killpg if target.startswith("group") else os.kill)(pid, sig)
        if target == "group-twice" and await_file("second"):
            os.killpg(pid, sig)
            time.sleep(0.3)
            os.remove(os.path.join(work, "second"))

deadline = time.time() + 120
while time.time() < deadline:
    done, status = os.waitpid(pid, os.WNOHANG)
    if done:
        if ending.startswith("tty"):
            with open(os.path.join(work, "job-status")) as f:
                print(f.read())
        else:
            print(os.waitstatus_to_exitcode(status))
        sys.exit(0)
    time.sleep(0.05)
for group in [pid] + ([int(open(os.path.join(work, "job-pid")).read())] if exists("job-pid") else []):
    try:
        os.killpg(group, signal.SIGKILL)
    except OSError:
        pass
os.waitpid(pid, 0)
print("timeout")
PY

# run_ending SHAPE ENDING [VAR=VALUE...] — run one gate to its end and print
# rc|first failure event|status file's verdict|events, one line.
run_ending() {
	local shape="$1" ending="$2" rc
	shift 2
	rm -rf "${WORK}/workdir" "${WORK}/lease" "${WORK}/ready" "${WORK}/second" "${WORK}/job-status" "${WORK}/job-pid"
	: >"${WORK}/events"
	: >"${WORK}/progress.jsonl"
	: >"${WORK}/teardown.log"
	: >"${WORK}/gate.out"
	rc="$(env "$@" REPO_ROOT="${REPO_ROOT}" WORK="${WORK}" SHAPE="${shape}" \
		RELEASE_PROGRESS_FILE="${WORK}/progress.jsonl" \
		RELEASE_STATUS_FILE="${WORK}/status.json" \
		RELEASE_READINGS_FILE="" \
		RELEASE_LEASE_DIR="${WORK}/lease" \
		TEARDOWN_LOG="${WORK}/teardown.log" \
		VALIDATE_RELEASE_LIB_ONLY=1 \
		python3 "${WORK}/ending.py" "${ending}" "${WORK}/gate.sh" "${WORK}")"
	printf '%s|%s|%s|%s\n' "${rc}" \
		"$(jq -r 'select(.phase == "gate" and .state == "fail") | .detail' "${WORK}/progress.jsonl" | head -1)" \
		"$(jq -r .gate "${WORK}/status.json")" \
		"$(paste -sd, - <"${WORK}/events")"
}

echo "scripts/dogfood/validate-release-signal-test.sh"

# A failed step is the control: what every ending below has to match.
check "a failed step records the failure and tears down" \
	"1|gate exited 1|failed|diagnostics,e2e-stop,stop" "$(run_ending fail none)"

check "TERM to the process group" \
	"143|gate exited 143|failed|diagnostics,e2e-stop,stop" "$(run_ending block TERM-group)"
# Alone, a trapped TERM waits out the command the gate is running.
check "TERM to the gate alone, once its command ends" \
	"143|gate exited 143|failed|diagnostics,e2e-stop,stop" "$(run_ending block TERM-gate BLOCK_SECONDS=2)"
check "Ctrl-C (INT to the process group)" \
	"130|gate exited 130|failed|diagnostics,e2e-stop,stop" "$(run_ending block INT-group)"
check "HUP to the process group" \
	"129|gate exited 129|failed|diagnostics,e2e-stop,stop" "$(run_ending block HUP-group)"
check "HUP to the group while the gate is inside \$(…)" \
	"129|gate exited 129|failed|diagnostics,e2e-stop,stop" "$(run_ending subst HUP-group)"
check "a closed terminal, output to a file" \
	"129|gate exited 129|failed|diagnostics,e2e-stop,stop" "$(run_ending block tty-closed-file)"

# Every write to the terminal fails once it is gone, the stop stubs' included.
check "a closed terminal, output to that terminal" \
	"129|gate exited 129|failed|diagnostics,e2e-stop,stop" "$(run_ending block tty-closed)"
check "the closed terminal's teardown writes to TEARDOWN_LOG" "1 1" \
	"$(grep -c 'teardown continues here' "${WORK}/teardown.log") $(grep -c 'stopping (stop stub)' "${WORK}/teardown.log")"

# A shell closing with its terminal forwards a HUP of its own, so one can arrive
# while teardown runs; it must not end teardown between the stop scripts.
check "a second HUP while teardown runs" \
	"129|gate exited 129|failed|diagnostics,e2e-stop,stop" "$(run_ending block HUP-group-twice SECOND=1)"

# e2e_leg's `wait "${capture_pid}" || true`: bash starts the job with INT
# ignored and `|| true` drops wait's 130, so Ctrl-C there used to carry on and
# record `passed`.
check "Ctrl-C while e2e_leg waits on its sizing capture" \
	"130|gate exited 130|failed|diagnostics,e2e-stop,stop" "$(run_ending wait INT-group)"

# dragonfly_readings sets its own interrupt traps for its lanes, and clearing
# them on the way out used to clear the gate's with them.
check "TERM after the Dragonfly readings have returned" \
	"143|gate exited 143|failed|diagnostics,e2e-stop,stop" "$(run_ending readings TERM-group)"

# A lost lease TERMs the gate alone, and a trapped TERM waits for the command
# the gate is blocked in, so the renewer TERMs that command too. The step here
# outlasts the harness's two-minute deadline, so a renewer that TERMed only the
# gate reads as `timeout` rather than as a slow pass. The teardown then leaves
# the cluster to its new holder.
# take_lease — once the gate holds its lease and blocks, record another holder.
take_lease() (
	local now
	for _ in $(seq 1200); do
		[[ -e "${WORK}/ready" ]] && break
		command sleep 0.05
	done
	export RELEASE_LEASE_DIR="${WORK}/lease"
	# shellcheck source=scripts/dogfood/lib/lease.sh
	source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"
	# shellcheck source=scripts/dogfood/lib/lease-fake.sh
	source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"
	now="$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)"
	lease_fake_write p z c ci-runner-7/4242 "${now}" 600 "${now}" v9.9.9-rc.1
)
rm -f "${WORK}/ready"
take_lease &
taker=$!
check "a lost lease stops the gate mid-step and leaves the cluster alone" \
	"1|lease taken by another holder|failed|" \
	"$(run_ending lease-lost lease-lost RELEASE_LEASE_RENEW_INTERVAL=0.2 BLOCK_SECONDS=600)"
wait "${taker}"

if ((fails > 0)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
