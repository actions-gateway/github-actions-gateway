#!/usr/bin/env bash
#
# Unit tests for scripts/fetch/prepull-manifest-images.sh (Q1133). The manifest
# fetch is the one network call left on the e2e critical path on a cache miss,
# and a flat curl `--retry 5 --retry-delay 2` spent its whole budget in 12.7s of
# release-CDN 500s. So these pin what replaced it: every nonzero curl exit is
# retried, the schedule is exponential, jittered and capped, an exhausted budget
# exits with curl's code and leaves no cache entry behind, and a cache hit never
# fetches at all.
#
# curl, sleep and docker are stubbed on PATH, so none of it touches the network
# or waits. Runs under `make check` (via `make scripts-test`) and the
# CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/fetch/prepull-manifest-images.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/prepull-manifest-images-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

assert_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$want" == "$got" ]]; then
		pass "$name"
	else
		fail "$name" "want $want got $got"
	fi
}

cat > "$FIXTURE_DIR/manifest.yaml" << 'YAML'
spec:
  containers:
    - name: controller
      image: "quay.io/example/controller:v1.0.0"
    - name: webhook
      image: quay.io/example/webhook:v1.0.0
YAML

# curl: counts its calls and exits 22 — what a release-CDN 500/504 produces —
# until CURL_SUCCEED_ON is reached, then serves the fixture manifest to the -o
# path. 0 (the default) never succeeds, which is the exhausted-budget case.
cat > "$FIXTURE_DIR/bin/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
calls="$STUB_DIR/curl.calls"
printf '%s\n' "$*" >> "$calls"
n=$(wc -l < "$calls")
out=""
while (($#)); do
	if [[ "$1" == "-o" ]]; then
		out="$2"
	fi
	shift
done
if ((${CURL_SUCCEED_ON:-0} > 0 && n >= CURL_SUCCEED_ON)); then
	cp "$STUB_DIR/manifest.yaml" "$out"
	exit 0
fi
exit 22
STUB

# sleep: records the requested duration instead of waiting.
cat > "$FIXTURE_DIR/bin/sleep" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >> "$STUB_DIR/sleeps"
STUB

# docker: pull and load succeed; save writes the -o path so the cache entry is
# observable.
cat > "$FIXTURE_DIR/bin/docker" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$STUB_DIR/docker.calls"
if [[ "$1" == "save" && "$2" == "-o" ]]; then
	: > "$3"
fi
STUB

chmod +x "$FIXTURE_DIR/bin/curl" "$FIXTURE_DIR/bin/sleep" "$FIXTURE_DIR/bin/docker"
export STUB_DIR="$FIXTURE_DIR"
export PATH="$FIXTURE_DIR/bin:$PATH"
unset MANIFEST_FETCH_ATTEMPTS MANIFEST_FETCH_DELAY MANIFEST_FETCH_MAX_DELAY REGISTRY_MIRRORS

url='https://example.invalid/releases/download/v1.0.0/manifest.yaml'

run_rc=0
curl_calls=0
declare -a sleeps=()
# run_prepull CACHE_DIR — run the script against clean counters. Reads the
# CURL_SUCCEED_ON environment the caller has set.
run_prepull() {
	: > "$FIXTURE_DIR/curl.calls"
	: > "$FIXTURE_DIR/sleeps"
	: > "$FIXTURE_DIR/docker.calls"
	run_rc=0
	"$SCRIPT" example "$url" "$1" > /dev/null 2>&1 || run_rc=$?
	die_if_killed "prepull into $1" "$run_rc"
	curl_calls=$(wc -l < "$FIXTURE_DIR/curl.calls" | tr -d ' ')
	mapfile -t sleeps < "$FIXTURE_DIR/sleeps"
}

# --- a cache hit never fetches ----------------------------------------------

warm="$FIXTURE_DIR/warm"
mkdir -p "$warm"
: > "$warm/images.tar"
printf 'quay.io/example/controller:v1.0.0\n' > "$warm/images.txt"
run_prepull "$warm"
assert_eq 'cache hit exits 0' 0 "$run_rc"
assert_eq 'cache hit never fetches the manifest' 0 "$curl_calls"

# --- recovery: every nonzero curl exit is retried ---------------------------

cold="$FIXTURE_DIR/cold"
CURL_SUCCEED_ON=3 run_prepull "$cold"
assert_eq 'a 5xx exit is retried, not fatal' 0 "$run_rc"
assert_eq 'recovery on attempt 3 stops fetching' 3 "$curl_calls"
assert_eq 'recovery on attempt 3 sleeps twice' 2 "${#sleeps[@]}"
for f in images.tar images.txt manifest.yaml; do
	if [[ -f "$cold/$f" ]]; then
		pass "recovered fetch writes $f"
	else
		fail "recovered fetch writes $f" "missing $cold/$f"
	fi
done
assert_eq 'image list is extracted from the fetched manifest' \
	'quay.io/example/controller:v1.0.0 quay.io/example/webhook:v1.0.0' \
	"$(tr '\n' ' ' < "$cold/images.txt" | sed 's/ $//')"

# --- the schedule is exponential, jittered, and capped ----------------------

exhausted="$FIXTURE_DIR/exhausted"
CURL_SUCCEED_ON=0 run_prepull "$exhausted"
assert_eq "exhausted budget exits with curl's code" 22 "$run_rc"
assert_eq 'default budget is 6 attempts' 6 "$curl_calls"
assert_eq 'exhausted budget sleeps between attempts only' 5 "${#sleeps[@]}"
if [[ -e "$exhausted" ]]; then
	fail 'exhausted budget writes no cache entry' "found $exhausted"
else
	pass 'exhausted budget writes no cache entry'
fi

# Defaults: base 5 doubling to a 60s cap, so the pre-jitter bases are
# 5, 10, 20, 40, 60. Jitter adds 0..half the delay, so each sleep must land in
# [base, base + base/2] — the lower bound pins the doubling, the upper bound the
# jitter's ceiling and the cap.
expected_bases=(5 10 20 40 60)
schedule_ok=1
total=0
for i in "${!expected_bases[@]}"; do
	base="${expected_bases[$i]}"
	got="${sleeps[$i]:-}"
	if [[ ! "$got" =~ ^[0-9]+$ ]] || ((got < base || got > base + base / 2)); then
		fail "sleep $((i + 1)) within [$base, $((base + base / 2))]" "got '${got}'"
		schedule_ok=0
	else
		total=$((total + got))
	fi
done
if ((schedule_ok == 1)); then
	pass "schedule is 5,10,20,40,60 plus jitter (${total}s of backoff)"
fi

if ((fails > 0)); then
	printf '%d failure(s)\n' "$fails" >&2
	exit 1
fi
echo 'all prepull-manifest-images tests passed'
