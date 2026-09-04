#!/usr/bin/env bash
#
# Tests for scripts/docs/git-merge-script-index.sh.
#
# A merge driver decides file content during a merge, so the refusals matter as
# much as the resolutions: a driver that silently picks a side loses a registry
# row, and the missing row only surfaces later as a check-script-docs failure on
# someone else's branch. Every case here asserts one direction or the other, and
# the ambiguous ones are required to produce ordinary conflict markers.
#
# The last case runs a real `git rebase` with the driver installed and
# .gitattributes routing the file, because everything above it would still pass
# if the wiring were wrong.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
DRIVER="$REPO_ROOT/scripts/docs/git-merge-script-index.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/git-merge-script-index-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
LAST_RC=0

# page ROWS... — a minimal scripts/README.md: a group summary table whose first
# cell is an in-page anchor, then one per-group table of script rows.
page() {
	cat <<'HEAD'
# Scripts

| Group | What |
|---|---|
| [`agent/`](#agent) | hooks |

## agent

| Script | What it does |
|---|---|
HEAD
	printf '%s\n' "$@"
	printf '\nTrailing prose.\n'
}

# run_merge BASE OURS THEIRS — invoke the driver; result lands in $FIXTURE_DIR/out.
run_merge() {
	cp "$2" "$FIXTURE_DIR/out"
	set +e
	"$DRIVER" "$1" "$FIXTURE_DIR/out" "$3" 7 scripts/README.md 2>"$FIXTURE_DIR/err"
	LAST_RC=$?
	set -e
}

ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1"
	[[ -n "${2:-}" ]] && printf '     %s\n' "$2"
	fails=$((fails + 1))
}

expect_resolved() {
	local desc="$1"
	shift
	die_if_killed "$desc" "$LAST_RC"
	if ((LAST_RC != 0)); then
		bad "$desc" "driver reported a conflict (rc=$LAST_RC): $(head -1 "$FIXTURE_DIR/err")"
		return
	fi
	if grep -q '<<<<<<<' "$FIXTURE_DIR/out"; then
		bad "$desc" "left conflict markers"
		return
	fi
	local missing=""
	for want in "$@"; do
		grep -qF "$want" "$FIXTURE_DIR/out" || missing="$missing $want"
	done
	if [[ -z "$missing" ]]; then
		ok "$desc"
	else
		bad "$desc" "missing:$missing"
	fi
}

expect_absent() {
	local desc="$1" gone="$2"
	if ((LAST_RC == 0)) && ! grep -qF "$gone" "$FIXTURE_DIR/out"; then
		ok "$desc"
	else
		bad "$desc" "expected '$gone' to be gone (rc=$LAST_RC)"
	fi
}

expect_markers() {
	local desc="$1"
	if ((LAST_RC != 0)) && grep -q '<<<<<<<' "$FIXTURE_DIR/out"; then
		ok "$desc"
	else
		bad "$desc" "expected conflict markers, got rc=$LAST_RC"
	fi
}

# expect_fallback DESC REASON_SUBSTRING — the driver declined the keyed merge and
# handed the file to git's plain three-way merge. That merge may well succeed, in
# which case the driver exits 0: refusing the smart path is not the same as
# producing markers, and the observable contract is the reason on stderr.
expect_fallback() {
	local desc="$1" want="$2"
	if grep -qF "$want" "$FIXTURE_DIR/err"; then
		ok "$desc"
	else
		bad "$desc" "stderr did not mention '$want': $(head -1 "$FIXTURE_DIR/err")"
	fi
}

A='| [a.sh](agent/a.sh) | Does A. |'
B='| [b.sh](agent/b.sh) | Does B. |'
C='| [c.sh](agent/c.sh) | Does C. |'

# --- resolves what it is certain about ------------------------------------

page "$A" >"$FIXTURE_DIR/base"
page "$A" "$B" >"$FIXTURE_DIR/ours"
page "$A" "$C" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_resolved "adjacent adds from both sides both survive" "agent/b.sh" "agent/c.sh"

page "$A" "$B" >"$FIXTURE_DIR/base"
page "$A" >"$FIXTURE_DIR/ours"
page "$A" "$B" "$C" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_absent "a row deleted on one side stays deleted" "agent/b.sh"

page "$A" "$B" >"$FIXTURE_DIR/base"
page "$A" '| [b.sh](agent/b.sh) | Does B, revised. |' >"$FIXTURE_DIR/ours"
page "$A" "$B" "$C" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_resolved "a one-sided row edit is kept alongside the other side's add" "Does B, revised." "agent/c.sh"

# --- refuses what it is not ------------------------------------------------

page "$A" "$B" >"$FIXTURE_DIR/base"
page "$A" '| [b.sh](agent/b.sh) | Ours rewrote it. |' >"$FIXTURE_DIR/ours"
page "$A" '| [b.sh](agent/b.sh) | Theirs rewrote it. |' >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_markers "the same row changed on both sides gets markers"

page "$A" "$B" >"$FIXTURE_DIR/base"
page "$A" >"$FIXTURE_DIR/ours"
page "$A" '| [b.sh](agent/b.sh) | Theirs edited it. |' >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_markers "delete on one side, edit on the other gets markers"

page "$A" >"$FIXTURE_DIR/base"
{ page "$A"; printf '\n| Extra | Table |\n|---|---|\n| [d.sh](agent/d.sh) | D. |\n'; } >"$FIXTURE_DIR/ours"
page "$A" "$C" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_fallback "a side that adds a whole table is refused, not merged by key" \
	"disagree on how many tables"

# Prose, not rows: both sides rewriting the same sentence is git's conflict.
page "$A" >"$FIXTURE_DIR/base"
{ page "$A" | sed 's/^Trailing prose\./Ours prose./'; } >"$FIXTURE_DIR/ours"
{ page "$A" | sed 's/^Trailing prose\./Theirs prose./'; } >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_markers "conflicting prose outside the tables gets markers"

# --- the wiring, end to end ------------------------------------------------

REPO="$FIXTURE_DIR/e2e"
mkdir -p "$REPO/scripts"
(
	cd "$REPO"
	git init -q -b main
	# Q820: no detached maintenance racing the next command in a fixture repo.
	git config maintenance.auto false
	git config user.email t@example.com
	git config user.name Test
	git config "merge.scriptindex.name" 'test'
	git config "merge.scriptindex.driver" "$DRIVER %O %A %B %L %P %S %X %Y"
	printf 'scripts/README.md merge=scriptindex\n' >.gitattributes
	page "$A" >scripts/README.md
	git add -A
	git commit -qm base
	git checkout -q -b topic
	page "$A" "$B" >scripts/README.md
	git commit -qam "topic adds b"
	git checkout -q main
	page "$A" "$C" >scripts/README.md
	git commit -qam "main adds c"
	git checkout -q topic
) >/dev/null 2>&1

set +e
(cd "$REPO" && git rebase main) >"$FIXTURE_DIR/rebase.log" 2>&1
rebase_rc=$?
die_if_killed "a real git rebase resolves adjacent adds" "$rebase_rc"
set -e
if ((rebase_rc == 0)) && grep -qF 'agent/b.sh' "$REPO/scripts/README.md" && grep -qF 'agent/c.sh' "$REPO/scripts/README.md"; then
	ok "a real git rebase resolves adjacent adds through .gitattributes"
else
	bad "a real git rebase resolves adjacent adds through .gitattributes" \
		"rc=$rebase_rc; $(tail -2 "$FIXTURE_DIR/rebase.log" | tr '\n' ' ')"
fi

# --- no background git in a fixture repo --------------------------------------

# Q820's cause, asserted on behaviour rather than on the config key that
# currently delivers it: a commit in a fixture repo must spawn nothing that
# outlives it. Dropping the `maintenance.auto false` call turns this red.
MAINT_REPO="$FIXTURE_DIR/maintenance"
mkdir -p "$MAINT_REPO"
(
	cd "$MAINT_REPO"
	git init -q -b main
	git config maintenance.auto false
	git config user.email t@example.com
	git config user.name Test
	printf 'x\n' >f
	git add -A
	git commit -qm base
) >/dev/null 2>&1
printf 'y\n' >"$MAINT_REPO/f"
MAINT_TRACE="$FIXTURE_DIR/maintenance-trace.log"
GIT_TRACE=1 git -C "$MAINT_REPO" commit -qam next >"$MAINT_TRACE" 2>&1
if grep -q 'maintenance run' "$MAINT_TRACE"; then
	bad "a fixture commit spawned background maintenance" \
		"$(grep -m1 -o 'git maintenance run.*' "$MAINT_TRACE")"
else
	ok 'a fixture commit spawns no detached maintenance'
fi

if ((fails > 0)); then
	echo "git-merge-script-index-test: ${fails} failure(s)"
	exit 1
fi
echo "git-merge-script-index-test: all assertions passed"
