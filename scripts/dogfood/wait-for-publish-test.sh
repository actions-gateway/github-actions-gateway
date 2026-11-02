#!/usr/bin/env bash
# wait-for-publish-test.sh — asserts the CI gate's wait on publish.yml, against a
# scripted `gh` and a no-op `sleep`.
#
# The subject has one failure that matters: returning 0 without a successful
# publish. So every path that is not `success` — a red publish, a cancelled one,
# a wait that runs out with the run still going, and one that runs out with no
# run at all — is posed here and must return non-zero.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/wait-for-publish.sh"

pass=0
fail=0
ok() { printf '[wait-for-publish-test] ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '[wait-for-publish-test] FAIL %s\n' "$1" >&2; fail=$((fail + 1)); }

WORK="$(mktemp -d)"
BIN="$WORK/bin"
REPO_DIR="$WORK/repo"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$BIN" "$REPO_DIR"

git -C "$REPO_DIR" init -q
git -C "$REPO_DIR" config maintenance.auto false
git -C "$REPO_DIR" config gc.auto 0
git -C "$REPO_DIR" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m c
git -C "$REPO_DIR" tag v1.9.0-rc.3
SHA="$(git -C "$REPO_DIR" rev-parse HEAD)"

# gh serves one response per call from STUB_SEQ (a file of one-line JSON arrays,
# or the word FAIL), repeating the last. It records its arguments.
cat >"$BIN/gh" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$STUB_DIR/calls"
n="$(cat "$STUB_DIR/n" 2>/dev/null || echo 0)"
total="$(wc -l <"$STUB_SEQ" | tr -d ' ')"
line=$((n + 1 > total ? total : n + 1))
echo $((n + 1)) >"$STUB_DIR/n"
resp="$(sed -n "${line}p" "$STUB_SEQ")"
if [[ "$resp" == FAIL ]]; then echo "gh: HTTP 502" >&2; exit 1; fi
printf '%s\n' "$resp"
STUB_BODY
cat >"$BIN/sleep" <<'STUB_BODY'
#!/usr/bin/env bash
echo "$*" >>"$STUB_DIR/sleeps"
STUB_BODY
chmod +x "$BIN/gh" "$BIN/sleep"

NONE='[]'
RUNNING='[{"databaseId":7,"status":"in_progress","conclusion":"","url":"https://r/7"}]'
GREEN='[{"databaseId":7,"status":"completed","conclusion":"success","url":"https://r/7"}]'
RED='[{"databaseId":7,"status":"completed","conclusion":"failure","url":"https://r/7"}]'
CANCELLED='[{"databaseId":7,"status":"completed","conclusion":"cancelled","url":"https://r/7"}]'

# run NAME WANT_RC WANT_TEXT TAG RESPONSES... — WAIT_TIMEOUT=3, WAIT_INTERVAL=1 (four polls).
run() {
	local name="$1" want_rc="$2" want_text="$3" tag="$4" d="$WORK/$1" rc=0 out
	shift 4
	mkdir -p "$d"
	: >"$d/calls"
	printf '%s\n' "$@" >"$d/seq"
	out="$(cd "$REPO_DIR" && PATH="$BIN:$PATH" STUB_DIR="$d" STUB_SEQ="$d/seq" REPO=o/r \
		WAIT_TIMEOUT=3 WAIT_INTERVAL=1 "$SUBJECT" "$tag" 2>&1)" || rc=$?
	if [[ "$rc" -eq "$want_rc" && "$out" == *"$want_text"* ]]; then
		ok "$name"
	else
		bad "$name (rc=$rc, want $want_rc; want text '$want_text')"
		printf '       %s\n' "$out" >&2
	fi
}

run green-at-once 0 'succeeded' v1.9.0-rc.3 "$GREEN"
run waits-for-the-run-to-appear 0 'succeeded' v1.9.0-rc.3 "$NONE" "$NONE" "$GREEN"
run waits-for-the-run-to-finish 0 'succeeded' v1.9.0-rc.3 "$RUNNING" "$RUNNING" "$GREEN"
run survives-a-transient-gh-failure 0 'succeeded' v1.9.0-rc.3 FAIL "$GREEN"
run red-publish-fails 1 "concluded 'failure'" v1.9.0-rc.3 "$RED"
run cancelled-publish-fails 1 "concluded 'cancelled'" v1.9.0-rc.3 "$CANCELLED"
run red-after-waiting-fails 1 "concluded 'failure'" v1.9.0-rc.3 "$RUNNING" "$RED"
# The trap: a wait that runs out must not return green.
run timeout-with-run-still-going-fails 1 'timed out' v1.9.0-rc.3 "$RUNNING"
run timeout-with-no-run-fails 1 'timed out' v1.9.0-rc.3 "$NONE"
run timeout-with-gh-down-fails 1 'gh run list failed' v1.9.0-rc.3 FAIL
run unknown-tag-fails 1 'not in this checkout' v9.9.9-rc.1 "$GREEN"

# The run is looked up by the tag's commit and the push event.
if grep -qF -- "--event push --commit ${SHA}" "$WORK/green-at-once/calls"; then
	ok "looks the run up by event and the tag's commit"
else
	bad "looks the run up by event and the tag's commit"
fi

# It polls rather than spinning: four polls, three sleeps, then gives up.
if [[ "$(wc -l <"$WORK/timeout-with-run-still-going-fails/sleeps" | tr -d ' ')" -eq 3 ]]; then
	ok "sleeps between polls and not after the last"
else
	bad "sleeps between polls and not after the last"
fi

printf '[wait-for-publish-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
