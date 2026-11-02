#!/usr/bin/env bash
# check-runner-window-test.sh — asserts every direction of the runner-window check.
#
# The fixture is the real actions/runner release sequence around 2.338 (read
# 2026-11-01 from the releases API), because the failure modes are in its shape:
# a prerelease (2.338.1) between two stable releases, and a newest release
# (2.338.2) that must not restart the clock. Epochs are fixed so no case depends
# on today's date.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-runner-window.sh"

pass=0
fail=0
ok() { printf '[check-runner-window-test] ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '[check-runner-window-test] FAIL %s\n' "$1" >&2; fail=$((fail + 1)); }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FEED="$WORK/feed.json"
cat >"$FEED" <<'JSON'
[
 {"tag_name":"v2.338.2","published_at":"2026-10-26T14:45:12Z","prerelease":false,"draft":false},
 {"tag_name":"v2.338.1","published_at":"2026-10-22T14:14:46Z","prerelease":true,"draft":false},
 {"tag_name":"v2.338.0","published_at":"2026-10-06T13:55:11Z","prerelease":false,"draft":false},
 {"tag_name":"v2.337.0","published_at":"2026-08-26T14:33:29Z","prerelease":false,"draft":false},
 {"tag_name":"v2.336.0","published_at":"2026-07-20T17:45:55Z","prerelease":false,"draft":false}
]
JSON

# run NAME EXPECTED_RC EXPECTED_TEXT ARGS... — runs the subject, checks code and a line of output.
run() {
	local name="$1" want_rc="$2" want_text="$3" out rc=0
	shift 3
	out="$("$SUBJECT" "$@" 2>&1)" || rc=$?
	if [[ "$rc" -eq "$want_rc" && "$out" == *"$want_text"* ]]; then
		ok "$name"
	else
		bad "$name (rc=$rc, want $want_rc; want text '$want_text')"
		printf '       %s\n' "$out" >&2
	fi
}

D0=1793109311 # 2026-10-27T13:55:11Z, 21d0h after v2.338.0 was published
run day-21-is-red 1 'STALE' --pinned 2.337.0 --now "$D0" "$FEED"
run day-20-is-green 0 '20 days ago' --pinned 2.337.0 --now $((D0 - 1)) "$FEED"
run counts-from-first-newer-not-newest 1 'v2.338.0' --pinned 2.337.0 --now "$D0" "$FEED"
run days-remaining-reported 1 '9 days remain' --pinned 2.337.0 --now "$D0" "$FEED"
run warn-days-flag 1 'STALE' --pinned 2.337.0 --warn-days 5 --now "$D0" "$FEED"

# Pinned 2.338.0: the only stable newer release is 2.338.2 (10-26). The prerelease
# 2.338.1 (10-22) is earlier, so counting it would read 21 days at 11-12 where the
# stable release reads 17.
NOV12=1794441600
run prerelease-does-not-start-the-clock 0 'v2.338.2' --pinned 2.338.0 --now "$NOV12" "$FEED"

run newest-pin-is-ok 0 'newest stable release' --pinned 2.338.2 --now "$NOV12" "$FEED"
run pin-newer-than-feed-is-ok 0 'newest stable release' --pinned 2.999.0 --now "$NOV12" "$FEED"

# Numeric, not lexical, ordering: 2.10.0 is newer than 2.9.0.
cat >"$WORK/numeric.json" <<'JSON'
[{"tag_name":"v2.10.0","published_at":"2026-10-01T00:00:00Z","prerelease":false,"draft":false}]
JSON
run numeric-version-order 1 'v2.10.0' --pinned 2.9.0 --now 1793059200 "$WORK/numeric.json"

# Stdin is the production path (gh api | script).
out="$("$SUBJECT" --pinned 2.337.0 --now "$D0" - <"$FEED" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" -eq 1 && "$out" == *STALE* ]]; then
	ok "reads the feed from stdin"
else
	bad "reads the feed from stdin (rc=$rc)"
fi

# Could-not-measure is 2, never 1 or 0.
echo '[]' >"$WORK/empty.json"
echo '{"message":"API rate limit exceeded"}' >"$WORK/error.json"
echo 'not json' >"$WORK/garbage.json"
echo '[{"tag_name":"v2.338.0","prerelease":false,"draft":false}]' >"$WORK/no-ts.json"
run empty-feed-is-2 2 'could not read' --pinned 2.337.0 "$WORK/empty.json"
run error-object-is-2 2 'could not read' --pinned 2.337.0 "$WORK/error.json"
run garbage-is-2 2 'could not read' --pinned 2.337.0 "$WORK/garbage.json"
run missing-timestamp-is-2 2 'could not read' --pinned 2.337.0 "$WORK/no-ts.json"
run missing-file-is-2 2 'cannot read' --pinned 2.337.0 "$WORK/absent.json"
run bad-pin-is-2 2 'not X.Y.Z' --pinned v2 "$FEED"

# The default pin is names.go's, which is a real version.
run default-pin-parses 0 'actions/runner' --now "$NOV12" "$FEED"

printf '[check-runner-window-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
