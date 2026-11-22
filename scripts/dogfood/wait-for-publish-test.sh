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
# or the word FAIL), repeating the last. `gh run view` is served the same way from
# its own file and counter. It records its arguments.
cat >"$BIN/gh" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$STUB_DIR/calls"
seq="$STUB_SEQ"; ctr="$STUB_DIR/n"
if [[ "$1 $2" == "run view" ]]; then seq="$STUB_DIR/view"; ctr="$STUB_DIR/nv"; fi
n="$(cat "$ctr" 2>/dev/null || echo 0)"
total="$(wc -l <"$seq" | tr -d ' ')"
line=$((n + 1 > total ? total : n + 1))
echo $((n + 1)) >"$ctr"
resp="$(sed -n "${line}p" "$seq")"
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

# A `gh run view --json jobs` response: image jobs named as the matrix names them,
# then the job under test.
job() { printf '{"name":"%s","status":"%s","conclusion":"%s"}' "$1" "$2" "$3"; }
IMG_DONE="$(job 'publish (gmc)' completed success),$(job 'publish (agc)' completed success)"
VIEW_IMAGES_GREEN="{\"jobs\":[${IMG_DONE},$(job 'chart-publish' in_progress '')]}"
VIEW_IMAGES_PENDING="{\"jobs\":[${IMG_DONE},$(job 'publish (worker)' in_progress '')]}"
VIEW_IMAGE_RED="{\"jobs\":[${IMG_DONE},$(job 'publish (worker)' completed failure '')]}"
VIEW_IMAGE_CANCELLED="{\"jobs\":[${IMG_DONE},$(job 'publish (worker)' completed cancelled '')]}"
VIEW_NO_IMAGES='{"jobs":[{"name":"validated-candidate","status":"completed","conclusion":"failure"}]}'
VIEW_EMPTY='{"jobs":[]}'

# run NAME WANT_RC WANT_TEXT TAG RESPONSES... — WAIT_TIMEOUT=3, WAIT_INTERVAL=1 (four polls).
run() {
	local name="$1" want_rc="$2" want_text="$3" tag="$4" d="$WORK/$1" rc=0 out
	shift 4
	mkdir -p "$d"
	: >"$d/calls"
	printf '%s\n' "$@" >"$d/seq"
	printf '%s\n' "${VIEW:-$VIEW_EMPTY}" >"$d/view"
	out="$(cd "$REPO_DIR" && PATH="$BIN:$PATH" STUB_DIR="$d" STUB_SEQ="$d/seq" REPO=o/r \
		WAIT_TIMEOUT=3 WAIT_INTERVAL=1 "$SUBJECT" ${FLAG:+"$FLAG"} "$tag" 2>&1)" || rc=$?
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

# --images (Q1365): returns once the image jobs have succeeded, whatever
# chart-publish is doing, and still refuses every other state.
FLAG=--images
VIEW="$VIEW_IMAGES_GREEN" run images-green-chart-publish-running 0 'images of v1.9.0-rc.3 were published' v1.9.0-rc.3 "$RUNNING"
VIEW="$VIEW_IMAGES_GREEN" run images-green-chart-publish-red 0 'images of v1.9.0-rc.3 were published' v1.9.0-rc.3 "$RED"
VIEW="$VIEW_IMAGES_PENDING" run images-pending-times-out 1 'timed out' v1.9.0-rc.3 "$RUNNING"
VIEW="$VIEW_IMAGE_RED" run images-red-fails 1 'did not succeed' v1.9.0-rc.3 "$RUNNING"
VIEW="$VIEW_IMAGE_CANCELLED" run images-cancelled-fails 1 'did not succeed' v1.9.0-rc.3 "$CANCELLED"
VIEW="$VIEW_NO_IMAGES" run finished-run-without-images-fails 1 'no succeeded image jobs' v1.9.0-rc.3 "$RED"
VIEW="$VIEW_EMPTY" run no-jobs-yet-times-out 1 'timed out' v1.9.0-rc.3 "$RUNNING"
VIEW="$VIEW_EMPTY" run no-run-yet-times-out 1 'timed out' v1.9.0-rc.3 "$NONE"
FLAG=
# Without the flag a green image set does not stand in for the run.
VIEW="$VIEW_IMAGES_GREEN" run plain-form-needs-the-whole-run 1 "concluded 'failure'" v1.9.0-rc.3 "$RED"

# The run is looked up by the tag's commit and the push event.
if grep -qF -- "--event push --commit ${SHA} --branch v1.9.0-rc.3" "$WORK/green-at-once/calls"; then
	ok "looks the run up by event, the tag's commit and the tag's name"
else
	bad "looks the run up by event, the tag's commit and the tag's name"
fi

# It polls rather than spinning: four polls, three sleeps, then gives up.
if [[ "$(wc -l <"$WORK/timeout-with-run-still-going-fails/sleeps" | tr -d ' ')" -eq 3 ]]; then
	ok "sleeps between polls and not after the last"
else
	bad "sleeps between polls and not after the last"
fi

printf '[wait-for-publish-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
