#!/usr/bin/env bash
#
# Unit tests for scripts/fetch/pull-image-with-retry.sh (Q460). What matters is the
# retry SCHEDULE, since that is what the flake was: a fixed 5s delay retried six
# concurrent trivy shards in lockstep and burned the whole budget in ~95s. So
# these assert the three properties the fix rests on — the delay grows
# exponentially, every delay carries jitter (concurrent callers must not retry in
# the same second), and the growth is capped so a registry that is genuinely down
# still fails on a bounded schedule instead of hanging.
#
# `docker` and `sleep` are stubbed on PATH: the schedule is observable from the
# arguments the script passes to sleep, so none of this needs a daemon, a
# network, or any real waiting.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/fetch/pull-image-with-retry.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/pull-image-with-retry-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# --- stubs -----------------------------------------------------------------

# docker: counts its calls and fails until DOCKER_SUCCEED_ON is reached. 0 (the
# default) never succeeds, which is the exhausted-budget case.
cat > "$FIXTURE_DIR/bin/docker" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
calls="$STUB_DIR/docker.calls"
printf '%s\n' "$*" >> "$calls"
n=$(wc -l < "$calls")
if (( ${DOCKER_SUCCEED_ON:-0} > 0 && n >= DOCKER_SUCCEED_ON )); then
	exit 0
fi
exit 1
STUB

# sleep: records the requested duration instead of waiting.
cat > "$FIXTURE_DIR/bin/sleep" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >> "$STUB_DIR/sleeps"
STUB

chmod +x "$FIXTURE_DIR/bin/docker" "$FIXTURE_DIR/bin/sleep"

# Stubs shadow the real tools for this suite only; the script under test is what
# resolves them, so the override has to be on its PATH, not baked into it.
export STUB_DIR="$FIXTURE_DIR"
export PATH="$FIXTURE_DIR/bin:$PATH"

# run_pull RC_VAR — run the script against a clean set of counters. Reads the
# PULL_RETRY_* and DOCKER_SUCCEED_ON environment the caller has set.
run_rc=0
docker_calls=0
declare -a sleeps=()
run_pull() {
	rm -f "$FIXTURE_DIR/docker.calls" "$FIXTURE_DIR/sleeps"
	: > "$FIXTURE_DIR/docker.calls"
	: > "$FIXTURE_DIR/sleeps"
	run_rc=0
	"$SCRIPT" "$@" > /dev/null 2>&1 || run_rc=$?
	docker_calls=$(wc -l < "$FIXTURE_DIR/docker.calls" | tr -d ' ')
	mapfile -t sleeps < "$FIXTURE_DIR/sleeps"
}

assert_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$want" == "$got" ]]; then
		pass "$name"
	else
		fail "$name" "want $want got $got"
	fi
}

# --- usage -----------------------------------------------------------------

rc=0
"$SCRIPT" > /dev/null 2>&1 || rc=$?
assert_eq 'no image ref rejected' 2 "$rc"

# --- the pull is attempted, and a success stops the loop --------------------

DOCKER_SUCCEED_ON=1 run_pull example.invalid/img:tag
assert_eq 'first-attempt success exits 0' 0 "$run_rc"
assert_eq 'first-attempt success pulls once' 1 "$docker_calls"
assert_eq 'first-attempt success never sleeps' 0 "${#sleeps[@]}"

DOCKER_SUCCEED_ON=3 PULL_RETRY_ATTEMPTS=6 run_pull example.invalid/img:tag
assert_eq 'recovery on attempt 3 exits 0' 0 "$run_rc"
assert_eq 'recovery on attempt 3 stops pulling' 3 "$docker_calls"
assert_eq 'recovery on attempt 3 sleeps twice' 2 "${#sleeps[@]}"

# --- an unreachable registry fails clearly, on a bounded schedule -----------

PULL_RETRY_ATTEMPTS=6 run_pull example.invalid/img:tag
assert_eq 'exhausted budget exits 1' 1 "$run_rc"
assert_eq 'exhausted budget uses every attempt' 6 "$docker_calls"
assert_eq 'exhausted budget sleeps between attempts only' 5 "${#sleeps[@]}"

# --- the schedule is exponential, jittered, and capped ----------------------

# Defaults: base 5 doubling to a 60s cap, so the pre-jitter bases are
# 5, 10, 20, 40, 60. Jitter adds 0..half the delay, so each sleep must land in
# [base, base + base/2] — a lower bound that pins the doubling and an upper
# bound that pins the jitter's ceiling and the cap.
run_pull example.invalid/img:tag
expected_bases=(5 10 20 40 60)
assert_eq 'default budget is 6 attempts' 6 "$docker_calls"
schedule_ok=1
for i in "${!expected_bases[@]}"; do
	base="${expected_bases[$i]}"
	got="${sleeps[$i]:-}"
	if [[ ! "$got" =~ ^[0-9]+$ ]] || ((got < base || got > base + base / 2)); then
		fail "sleep $((i + 1)) within [$base, $((base + base / 2))]" "got '${got}'"
		schedule_ok=0
	fi
done
if ((schedule_ok == 1)); then
	pass "schedule doubles to the cap: ${sleeps[*]}"
fi

# The cap is what keeps a genuinely-down registry bounded rather than hanging:
# with the defaults the whole budget must stay well inside a CI job timeout.
total=0
for s in "${sleeps[@]}"; do
	total=$((total + s))
done
if ((total <= 300)); then
	pass "total backoff bounded (${total}s <= 300s)"
else
	fail 'total backoff bounded' "want <= 300s, got ${total}s"
fi

# A raised attempt count must not raise the per-sleep delay past the cap.
PULL_RETRY_ATTEMPTS=9 run_pull example.invalid/img:tag
capped=1
for s in "${sleeps[@]}"; do
	if ((s > 60 + 30)); then
		fail 'no sleep exceeds the cap plus its jitter' "got ${s}s"
		capped=0
		break
	fi
done
if ((capped == 1)); then
	pass 'no sleep exceeds the cap plus its jitter'
fi

# Jitter must actually vary, or six shards that failed together stay in lockstep
# for the whole budget. Sample the first delay repeatedly: with base 40 the
# jitter range is 0..20, so identical draws across 12 runs is a ~1e-15 event —
# a failure here means the jitter is gone, not a flake.
declare -A seen=()
for _ in {1..12}; do
	PULL_RETRY_ATTEMPTS=2 PULL_RETRY_DELAY=40 run_pull example.invalid/img:tag
	seen["${sleeps[0]:-none}"]=1
done
if ((${#seen[@]} > 1)); then
	pass "first delay is jittered (${#seen[@]} distinct draws in 12 runs)"
else
	fail 'first delay is jittered' "all 12 runs slept ${!seen[*]}s — jitter missing"
fi

# --- the registry mirror (Q408 Phase 3) -------------------------------------
#
# This script is the chokepoint every docker pull in the e2e job goes through,
# so it is where the job is pointed at the in-cluster pull-through cache. The
# contract it has to keep is that NOTHING DOWNSTREAM CAN TELL: the callers
# `docker tag`, `docker save` and `kind load` the ref they asked for, and
# prepull-manifest-images.sh writes that same ref into images.txt. A rewrite
# that pulled but left the image under the mirror's name would pass a pull and
# fail a `kind load` several steps later.

# docker_calls_matching PATTERN — how many recorded docker invocations match.
docker_calls_matching() {
	grep -c -- "$1" "$FIXTURE_DIR/docker.calls" || true
}

MIRROR=mirror-quay-io.gag-registry-mirror.svc.cluster.local:5000
QUAY_REF=quay.io/jetstack/cert-manager-controller:v1.20.2

DOCKER_SUCCEED_ON=1 run_pull "$QUAY_REF"
assert_eq 'no map: the pull uses the caller ref' 1 "$(docker_calls_matching "^pull $QUAY_REF\$")"
assert_eq 'no map: nothing is re-tagged' 0 "$(docker_calls_matching '^tag ')"

REGISTRY_MIRRORS="quay.io=$MIRROR" DOCKER_SUCCEED_ON=1 run_pull "$QUAY_REF"
assert_eq 'mapped: the pull goes to the mirror' \
	1 "$(docker_calls_matching "^pull $MIRROR/jetstack/cert-manager-controller:v1.20.2\$")"
assert_eq 'mapped: the caller ref is never pulled' 0 "$(docker_calls_matching "^pull $QUAY_REF\$")"
assert_eq 'mapped: the image is re-tagged to the caller ref' \
	1 "$(docker_calls_matching "^tag $MIRROR/jetstack/cert-manager-controller:v1.20.2 $QUAY_REF\$")"
assert_eq 'mapped: the mirror tag is removed again' \
	1 "$(docker_calls_matching "^rmi $MIRROR/jetstack/cert-manager-controller:v1.20.2\$")"
assert_eq 'mapped: a first-attempt success still exits 0' 0 "$run_rc"

# An unmapped registry under a configured map is the common case, not an edge
# one: the map covers five hosts and the job also pulls from the in-job local
# registry on 127.0.0.1:5000.
REGISTRY_MIRRORS="quay.io=$MIRROR" DOCKER_SUCCEED_ON=1 run_pull 127.0.0.1:5000/gmc:dev
assert_eq 'an unmapped registry is pulled direct' \
	1 "$(docker_calls_matching '^pull 127.0.0.1:5000/gmc:dev$')"
assert_eq 'and is not re-tagged' 0 "$(docker_calls_matching '^tag ')"

# The retry schedule is measured on the ref actually being pulled, so a mirror
# that is down must still exhaust the budget rather than fail on the first try.
REGISTRY_MIRRORS="quay.io=$MIRROR" PULL_RETRY_ATTEMPTS=3 run_pull "$QUAY_REF"
assert_eq 'a failing mirror pull exits 1' 1 "$run_rc"
assert_eq 'a failing mirror pull uses every attempt' 3 "$docker_calls"

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall pull-image-with-retry.sh assertions passed\n'
