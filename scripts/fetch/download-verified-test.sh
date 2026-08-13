#!/usr/bin/env bash
#
# Unit tests for scripts/fetch/download-verified.sh (Q433, Q829). The property
# that matters is that the retry can never become a path around the integrity
# check: a mismatched digest must fail and leave nothing at the output path, a
# malformed or absent digest must be rejected outright, there must be no flag or
# environment variable that skips verification, and a mismatch must not be
# retried at all.
#
# The retry SCHEDULE is asserted too, because that is what Q829 was: six flat 2s
# attempts burned the whole budget in 10.3s against a syft release 503. So these
# pin the same three properties the sibling's fix rests on — exponential growth,
# jitter on every delay (concurrent matrix shards must not retry in the same
# second), and a cap — plus the retry covering a 4xx at all, which is the Q433
# regression, now a property of the loop rather than of `--retry-all-errors`.
#
# Downloads use file:// URLs, so these assertions need no network; the schedule
# half stubs `curl` and `sleep` on PATH, so none of it waits.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/fetch/download-verified.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/download-verified-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

# Keep a failing download from burning the default retry budget. The schedule
# section below drops these so the defaults are what it measures.
export DOWNLOAD_RETRIES=0
export DOWNLOAD_RETRY_DELAY=0

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# assert_rc NAME WANT_RC ARGS... — run the script and assert its exit code.
assert_rc() {
	local name="$1" want_rc="$2" got_rc=0
	shift 2
	"$SCRIPT" "$@" > /dev/null 2>&1 || got_rc=$?
	if [[ "$got_rc" == "$want_rc" ]]; then
		pass "$name (rc=$got_rc)"
	else
		fail "$name" "want rc=$want_rc got rc=$got_rc"
	fi
}

# assert_fails NAME ARGS... — assert the script exits nonzero. Used where the
# code comes from curl and varies by scheme (403 over https exits 22, an
# unreadable file:// URL exits 37); only the failure itself is the contract.
assert_fails() {
	local name="$1" got_rc=0
	shift
	"$SCRIPT" "$@" > /dev/null 2>&1 || got_rc=$?
	if ((got_rc != 0)); then
		pass "$name (rc=$got_rc)"
	else
		fail "$name" 'want nonzero, got rc=0'
	fi
}

sha256_of() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

src="$FIXTURE_DIR/asset.bin"
printf 'pinned release asset\n' > "$src"
src_sha="$(sha256_of "$src")"
src_url="file://$src"
wrong_sha="$(printf '0%.0s' {1..64})"

# --- usage / argument validation ------------------------------------------

assert_rc 'no args rejected' 2
assert_rc 'missing output path rejected' 2 "$src_url" "$src_sha"
assert_rc 'empty digest rejected' 2 "$src_url" '' "$FIXTURE_DIR/out-empty.bin"
assert_rc 'short digest rejected' 2 "$src_url" 'deadbeef' "$FIXTURE_DIR/out-short.bin"
assert_rc 'non-hex digest rejected' 2 "$src_url" \
	"$(printf 'z%.0s' {1..64})" "$FIXTURE_DIR/out-nonhex.bin"

# --- happy path ------------------------------------------------------------

out="$FIXTURE_DIR/out-ok.bin"
assert_rc 'matching digest succeeds' 0 "$src_url" "$src_sha" "$out"
if cmp -s "$src" "$out"; then
	pass 'verified download has the source bytes'
else
	fail 'verified download has the source bytes' "content differs or missing: $out"
fi

# An uppercase digest is the same digest; the comparison must not care.
assert_rc 'uppercase digest accepted' 0 "$src_url" \
	"$(printf '%s' "$src_sha" | tr '[:lower:]' '[:upper:]')" "$FIXTURE_DIR/out-upper.bin"

# A missing parent directory is created rather than failing the move.
assert_rc 'missing output dir created' 0 "$src_url" "$src_sha" \
	"$FIXTURE_DIR/nested/dir/out.bin"

# --- integrity failures leave nothing behind -------------------------------

out_bad="$FIXTURE_DIR/out-mismatch.bin"
assert_rc 'digest mismatch fails' 1 "$src_url" "$wrong_sha" "$out_bad"
if [[ -e "$out_bad" ]]; then
	fail 'digest mismatch writes no output' "unverified bytes left at $out_bad"
else
	pass 'digest mismatch writes no output'
fi

out_missing="$FIXTURE_DIR/out-404.bin"
assert_fails 'unfetchable url fails' "file://$FIXTURE_DIR/no-such-asset.bin" \
	"$src_sha" "$out_missing"
if [[ -e "$out_missing" ]]; then
	fail 'failed download writes no output' "file left at $out_missing"
else
	pass 'failed download writes no output'
fi

# --- the retry: what it covers, and on what schedule ------------------------

# curl and sleep are stubbed on PATH from here down: the schedule is observable
# from the arguments the script passes to sleep, so none of it waits.
mkdir -p "$FIXTURE_DIR/bin"

# curl: counts its calls and exits 22 — curl's HTTP-error code, what a
# releases-CDN 403/503 produces, and what curl's own --retry ignores — until
# CURL_SUCCEED_ON is reached, at which point it serves the fixture asset to the
# -o path. 0 (the default) never succeeds, which is the exhausted-budget case.
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
	cp "$STUB_DIR/asset.bin" "$out"
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

chmod +x "$FIXTURE_DIR/bin/curl" "$FIXTURE_DIR/bin/sleep"

# Stubs shadow the real tools for this section only; the script under test is
# what resolves them, so the override has to be on its PATH, not baked into it.
export STUB_DIR="$FIXTURE_DIR"
export PATH="$FIXTURE_DIR/bin:$PATH"

# The defaults are the subject from here down, so stop pinning them to 0.
unset DOWNLOAD_RETRIES DOWNLOAD_RETRY_DELAY

# run_download ARGS... — run the script against a clean set of counters. Reads
# the DOWNLOAD_RETRY_* and CURL_SUCCEED_ON environment the caller has set.
run_rc=0
curl_calls=0
declare -a sleeps=()
run_download() {
	: > "$FIXTURE_DIR/curl.calls"
	: > "$FIXTURE_DIR/sleeps"
	run_rc=0
	"$SCRIPT" "$@" > /dev/null 2>&1 || run_rc=$?
	curl_calls=$(wc -l < "$FIXTURE_DIR/curl.calls" | tr -d ' ')
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

stub_url='https://example.invalid/pinned-asset.tar.gz'

CURL_SUCCEED_ON=1 run_download "$stub_url" "$src_sha" "$FIXTURE_DIR/out-stub-ok.bin"
assert_eq 'first-attempt success exits 0' 0 "$run_rc"
assert_eq 'first-attempt success downloads once' 1 "$curl_calls"
assert_eq 'first-attempt success never sleeps' 0 "${#sleeps[@]}"

# The Q433 property, as behaviour rather than as a flag: curl exits 22 on the
# CDN's 403/503, which curl's own --retry would not have retried, so a recovery
# on a later attempt is only possible if the loop retries every nonzero exit.
out_recovered="$FIXTURE_DIR/out-recovered.bin"
CURL_SUCCEED_ON=3 run_download "$stub_url" "$src_sha" "$out_recovered"
assert_eq 'a 4xx/5xx exit is retried, not fatal' 0 "$run_rc"
assert_eq 'recovery on attempt 3 stops downloading' 3 "$curl_calls"
assert_eq 'recovery on attempt 3 sleeps twice' 2 "${#sleeps[@]}"
if cmp -s "$src" "$out_recovered"; then
	pass 'recovered download is still digest-verified'
else
	fail 'recovered download is still digest-verified' "content differs or missing: $out_recovered"
fi

# A mismatch is not transient, so it must fail on the spot rather than spend the
# budget re-fetching bytes that cannot become the pinned ones.
out_stub_bad="$FIXTURE_DIR/out-stub-mismatch.bin"
CURL_SUCCEED_ON=1 run_download "$stub_url" "$wrong_sha" "$out_stub_bad"
assert_eq 'digest mismatch exits 1' 1 "$run_rc"
assert_eq 'digest mismatch is not retried' 1 "$curl_calls"
assert_eq 'digest mismatch never sleeps' 0 "${#sleeps[@]}"

# --- the schedule is exponential, jittered, and capped ----------------------

out_exhausted="$FIXTURE_DIR/out-exhausted.bin"
run_download "$stub_url" "$src_sha" "$out_exhausted"
assert_eq "exhausted budget exits with curl's code" 22 "$run_rc"
assert_eq 'default budget is 6 attempts' 6 "$curl_calls"
assert_eq 'exhausted budget sleeps between attempts only' 5 "${#sleeps[@]}"
if [[ -e "$out_exhausted" ]]; then
	fail 'exhausted budget writes no output' "file left at $out_exhausted"
else
	pass 'exhausted budget writes no output'
fi

# Defaults: base 5 doubling to a 60s cap, so the pre-jitter bases are
# 5, 10, 20, 40, 60. Jitter adds 0..half the delay, so each sleep must land in
# [base, base + base/2] — a lower bound that pins the doubling and an upper
# bound that pins the jitter's ceiling and the cap.
expected_bases=(5 10 20 40 60)
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

# Q829 was a budget that ran out before the denial did: the whole schedule has
# to outlast a CDN brown-out, while staying well inside every caller's job
# timeout (the tightest is the 5-minute shellcheck job in unit-test.yml).
total=0
for s in "${sleeps[@]}"; do
	total=$((total + s))
done
if ((total >= 120 && total <= 300)); then
	pass "total backoff within [120s, 300s] (${total}s)"
else
	fail 'total backoff within [120s, 300s]' "got ${total}s"
fi

# A raised retry count must not raise the per-sleep delay past the cap.
DOWNLOAD_RETRIES=8 run_download "$stub_url" "$src_sha" "$FIXTURE_DIR/out-capped.bin"
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

# Jitter must actually vary, or shards denied in the same second stay in lockstep
# for the whole budget. Sample the first delay repeatedly: with base 40 the
# jitter range is 0..20, so identical draws across 12 runs is a ~1e-15 event —
# a failure here means the jitter is gone, not a flake.
declare -A seen=()
for _ in {1..12}; do
	DOWNLOAD_RETRIES=1 DOWNLOAD_RETRY_DELAY=40 \
		run_download "$stub_url" "$src_sha" "$FIXTURE_DIR/out-jitter.bin"
	seen["${sleeps[0]:-none}"]=1
done
if ((${#seen[@]} > 1)); then
	pass "first delay is jittered (${#seen[@]} distinct draws in 12 runs)"
else
	fail 'first delay is jittered' "all 12 runs slept ${!seen[*]}s — jitter missing"
fi

# Nothing may bypass the digest comparison: it is the only writer of the output
# path, and it is unconditional.
# shellcheck disable=SC2016  # matching the script's source text, not expanding it
if grep -q 'if \[\[ "$got" != "$want" \]\]; then' "$SCRIPT"; then
	pass 'digest comparison is unconditional'
else
	fail 'digest comparison is unconditional' "comparison not found in $SCRIPT"
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall download-verified.sh assertions passed\n'
