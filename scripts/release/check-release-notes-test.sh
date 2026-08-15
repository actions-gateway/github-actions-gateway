#!/usr/bin/env bash
# check-release-notes-test.sh — every rule fires, and none fires on a clean note.
#
# A release-body gate that never failed would pass every release and catch
# nothing, and one that failed a good note would be switched off within a cycle.
# Each rule below is therefore asserted in both directions against a fixture whose
# answer is known by construction, plus a reconciliation that the notes actually
# shipped still pass.
# Fixtures below are markdown, so single-quoted strings are full of backticks
# and `$` that must stay literal. Disabled file-wide rather than per case.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-release-notes.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok() {
	printf '[check-release-notes-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-release-notes-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}
expect() {
	local desc="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc (want ${want}, got ${got})"
	fi
}

# case DESC WANT_EXIT BODY
case_() {
	local desc="$1" want="$2" body="$3" rc=0 out
	printf '%s' "$body" >"$WORK/note.md"
	out="$("$SUBJECT" "$WORK/note.md" 2>&1)" || rc=$?
	if [[ "$rc" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc (want exit $want, got $rc)"
		printf '       %s\n' "$out" >&2
	fi
}

CLEAN='## Highlights

A claim that holds.

- A bullet.

```bash
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway --version 1.5.0
```

See [upgrade](https://actions-gateway.com/1.5.0/operations/upgrade/).
'

case_ "a clean note passes" 0 "$CLEAN"

# --- each rule fires ---------------------------------------------------------
case_ "a top-level heading duplicates the page h1" 1 '# v1.5.0

Body.
'

case_ "an in-page anchor is dead in a release body" 1 '## Upgrading

Jump to [Upgrading](#upgrading).
'

case_ "a v-prefixed chart version fails the helm command" 1 '## Install

```bash
helm upgrade gag oci://ghcr.io/actions-gateway/charts/actions-gateway --version v1.5.0
```
'

case_ "the = form of the version flag is caught too" 1 '## Install

`helm upgrade gag --version=v1.5.0`
'

# --- and none fires where it should not --------------------------------------
#
# The exclusions matter as much as the rules: a gate that flagged these would be
# wrong about correct notes, which is how a gate stops being read.
case_ "a heading inside a fenced block is not a duplicate h1" 0 '## Notes

```bash
# this is a shell comment, not a heading
echo hi
```
'

case_ "an anchor on another document is fine" 0 '## Upgrading

See [rollback](https://actions-gateway.com/1.5.0/operations/upgrade/#gmc-rollback).
'

case_ "an image tag keeps its v" 0 '## Images

`ghcr.io/actions-gateway/gmc:v1.5.0` is the image tag.
'

# --- collapsed height is reported, never failed ------------------------------
#
# A note far past the watermark must still exit 0: GitHub publishes no threshold,
# so failing on an invented one would block a release over a guess.
big="## Highlights

"
for i in $(seq 1 400); do big+="Line ${i} of a very long unfolded section that pushes collapsed height well past the watermark.
"; done
case_ "a body over the watermark warns without failing" 0 "$big"

# Captured, not piped into grep: `grep -q` closes the pipe on its first match, and
# under pipefail the producer's SIGPIPE (141) becomes the pipeline's status — so a
# successful match reads as a failure.
printf '%s' "$big" >"$WORK/note.md"
warn_out="$("$SUBJECT" "$WORK/note.md" 2>&1 || true)"
case "$warn_out" in
*"warning: above the"*) ok "the watermark warning is actually emitted" ;;
*) bad "the watermark warning is actually emitted" ;;
esac
case "$warn_out" in
*"Highlights"*) ok "the warning ranks the sections to fold" ;;
*) bad "the warning ranks the sections to fold" ;;
esac

# --- usage and reconciliation ------------------------------------------------
rc=0
"$SUBJECT" "$WORK/absent.md" >/dev/null 2>&1 || rc=$?
expect "a missing file is exit 2" 2 "$rc"

# The notes this repo has already published must pass, or the gate is describing
# a convention the project does not actually follow.
rc=0
out="$("$SUBJECT" 2>&1)" || rc=$?
if [[ "$rc" == 0 ]]; then
	ok "every shipped release note passes"
else
	bad "every shipped release note passes"
	printf '       %s\n' "$out" >&2
fi

printf '[check-release-notes-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
