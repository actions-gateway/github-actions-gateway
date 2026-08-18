#!/usr/bin/env bash
#
# Tests for scripts/ci/git-merge-gate-lists.sh.
#
# This driver rewrites part of mk/gate-lists.mk during a merge, so two
# properties are asserted on every resolution rather than just one: the merged
# entry set is exactly right, AND `make` still parses the file. A driver that
# produces a correct-looking list with a dropped backslash breaks every build
# downstream, and a set assertion alone would not see it.
#
# The zero-churn case is asserted too. An earlier revision re-rendered every
# managed list on every merge, which rewrapped 30 lines of a variable neither
# side had touched; that is noise in every future merge and it buries the real
# change during review.
#
# EVERY FIXTURE HERE IS DERIVED FROM mk/gate-lists.mk, never restated. The
# driver requires each variable it manages to be assigned on all three sides, so
# a fixture carrying its own variable list asserts the shape it was written
# against rather than the shape the repo has. That is not hypothetical: the
# driver named STATUS_GATES after mk/gate-lists.mk stopped assigning it, which
# made it refuse every merge of that file, and this suite passed throughout
# because its fixture still declared STATUS_GATES (Q915).
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
DRIVER="$REPO_ROOT/scripts/ci/git-merge-gate-lists.sh"
GATE_LISTS="$REPO_ROOT/mk/gate-lists.mk"

FIXTURE_DIR="$REPO_ROOT/tmp/git-merge-gate-lists-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
LAST_RC=0

ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1"
	[[ -n "${2:-}" ]] && printf '     %s\n' "$2"
	fails=$((fails + 1))
}

contains() {
	local needle="$1" e
	shift
	for e in "$@"; do
		if [[ "$e" == "$needle" ]]; then
			return 0
		fi
	done
	return 1
}

# --- what the driver manages, and what the file assigns --------------------

# The driver reports its own array rather than this suite parsing it out of the
# source, so a reformatted MANAGED_VARS cannot make the reconciliation below
# read a list nobody runs on.
mapfile -t MANAGED < <("$DRIVER" --managed-vars)
if ((${#MANAGED[@]} == 0)); then
	echo "FAIL the driver reports the variables it manages"
	echo "     $DRIVER --managed-vars produced nothing"
	exit 1
fi

# The same rule lift_vars opens an assignment with, so the two agree on what
# counts as one.
mapfile -t ASSIGNED < <(awk '
	/^[A-Z_]+[ \t]*[:+?]?=/ { name = $0; sub(/[ \t]*[:+?]?=.*$/, "", name); print name }
' "$GATE_LISTS")
if ((${#ASSIGNED[@]} < 3)); then
	echo "FAIL mk/gate-lists.mk assigns enough lists to build a fixture from"
	echo "     found ${#ASSIGNED[@]}; this suite needs three distinct lists"
	exit 1
fi

# Three roles, taken from the file in order. The driver treats every managed
# list alike, so which name plays which part does not matter — only that the
# names are the file's own.
WRAPPED_VAR="${ASSIGNED[0]}"   # written with continuations, for the churn case
DROP_VAR="${ASSIGNED[1]}"      # removed from one side, for the refusal case
SUBJECT_VAR="${ASSIGNED[-1]}"  # carries the entries every merge assertion is about

# --- the driver's list against the file it merges --------------------------
#
# Both directions, because the two failures are different and both silent. A
# name the driver manages that the file does not assign makes lift_vars
# hard-fail, so the driver refuses every merge and git leaves ordinary conflict
# markers. A list the file assigns that the driver does not manage is merged by
# git alone, so two PRs appending to it collide on adjacent lines — the conflict
# this driver exists to remove.

stale=()
for v in "${MANAGED[@]}"; do
	contains "$v" "${ASSIGNED[@]}" || stale+=("$v")
done
if ((${#stale[@]} == 0)); then
	ok "every variable the driver manages is assigned in mk/gate-lists.mk"
else
	bad "every variable the driver manages is assigned in mk/gate-lists.mk" \
		"MANAGED_VARS names ${stale[*]}, which mk/gate-lists.mk does not assign; the driver refuses every merge of that file until the two agree"
fi

unmanaged=()
for v in "${ASSIGNED[@]}"; do
	contains "$v" "${MANAGED[@]}" || unmanaged+=("$v")
done
if ((${#unmanaged[@]} == 0)); then
	ok "every list in mk/gate-lists.mk is one the driver manages"
else
	bad "every list in mk/gate-lists.mk is one the driver manages" \
		"mk/gate-lists.mk assigns ${unmanaged[*]}, which MANAGED_VARS omits; appends to those still conflict by line position"
fi

# --- fixtures ---------------------------------------------------------------

# makefile ENTRIES — a makefile assigning every variable mk/gate-lists.mk
# assigns, with ENTRIES as $SUBJECT_VAR's list. $WRAPPED_VAR is written over a
# continuation so the churn case has wrapped lines to compare.
makefile() {
	local v
	echo "# leading prose"
	for v in "${ASSIGNED[@]}"; do
		echo
		case "$v" in
		"$SUBJECT_VAR") printf '%s := %s\n' "$v" "$1" ;;
		"$WRAPPED_VAR") printf '%s := filler-a filler-b \\\n                    filler-c\n' "$v" ;;
		*) printf '%s := filler-a filler-b\n' "$v" ;;
		esac
	done
	printf '\n.PHONY: all\nall:\n\t@echo hi\n'
}

run_merge() {
	cp "$2" "$FIXTURE_DIR/out"
	set +e
	"$DRIVER" "$1" "$FIXTURE_DIR/out" "$3" 7 mk/gate-lists.mk 2>"$FIXTURE_DIR/err"
	LAST_RC=$?
	set -e
}

# entries_of FILE VAR — read the variable back through make's own parser, so a
# broken continuation shows up as a parse failure rather than a passing string
# comparison. `make --eval` is not available on the make shipped with macOS, so
# a wrapper makefile includes the file under test.
#
# --no-print-directory is load-bearing, not tidiness. Under `make scripts-test`
# this suite is a sub-make, and GNU make then writes "Entering directory ..." to
# stdout, whose words land in the entry list and fail every set comparison. It
# passed locally at top level and failed on CI for exactly that reason.
entries_of() {
	local file="$1" var="$2"
	# The `$(...)` here is Make's expansion syntax and must reach the generated
	# makefile literally, so the single quotes are the point.
	# shellcheck disable=SC2016
	printf 'include %s\n__l:\n\t@echo $(%s)\n' "$file" "$var" >"$FIXTURE_DIR/wrap.mk"
	make --no-print-directory -f "$FIXTURE_DIR/wrap.mk" __l 2>"$FIXTURE_DIR/make.err" | tr ' ' '\n' | sed '/^$/d' | sort
}

expect_set() {
	local desc="$1" var="$2"
	shift 2
	if ((LAST_RC != 0)); then
		bad "$desc" "driver reported a conflict: $(head -1 "$FIXTURE_DIR/err")"
		return
	fi
	local got want
	got="$(entries_of "$FIXTURE_DIR/out" "$var")"
	if [[ -s "$FIXTURE_DIR/make.err" ]]; then
		bad "$desc" "make could not parse the merged file: $(head -1 "$FIXTURE_DIR/make.err")"
		return
	fi
	want="$(printf '%s\n' "$@" | sort)"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc" "got [$(echo "$got" | tr '\n' ' ')] want [$(echo "$want" | tr '\n' ' ')]"
	fi
}

expect_fallback() {
	local desc="$1" want="$2"
	if grep -qF "$want" "$FIXTURE_DIR/err"; then
		ok "$desc"
	else
		bad "$desc" "stderr did not mention '$want': $(head -1 "$FIXTURE_DIR/err")"
	fi
}

# --- resolves what it is certain about ------------------------------------

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test b-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "adjacent appends from both sides both survive" "$SUBJECT_VAR" \
	a-test b-test ours-test theirs-test

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "an entry deleted on one side stays deleted" "$SUBJECT_VAR" \
	a-test theirs-test

# The plain three-way merge already handles a one-sided change; what matters is
# that routing the file through this driver does not lose the other side's work.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "a one-sided append is kept" "$SUBJECT_VAR" a-test ours-test

# --- churn ----------------------------------------------------------------

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test b-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
if diff <(grep -A2 "^$WRAPPED_VAR" "$FIXTURE_DIR/base") \
	<(grep -A2 "^$WRAPPED_VAR" "$FIXTURE_DIR/out") >/dev/null; then
	ok "a list neither side touched is left byte for byte"
else
	bad "a list neither side touched is left byte for byte" \
		"$(diff <(grep -A2 "^$WRAPPED_VAR" "$FIXTURE_DIR/base") <(grep -A2 "^$WRAPPED_VAR" "$FIXTURE_DIR/out") | head -4 | tr '\n' ' ')"
fi

# --- refuses what it is not ------------------------------------------------

# A conflict elsewhere in the file is an ordinary makefile conflict.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test" | sed 's/^# leading prose/# ours prose/' >"$FIXTURE_DIR/ours"
makefile "a-test" | sed 's/^# leading prose/# theirs prose/' >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_fallback "a conflict outside the gate lists is refused, not resolved" \
	"conflicts outside the gate lists"

# A side missing a managed variable breaks the pairing the driver depends on.
# This is the shape the repo itself was in: the name the driver looked for was
# not in the file, and every merge fell back to ordinary markers.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test" | grep -v "^$DROP_VAR" >"$FIXTURE_DIR/ours"
makefile "a-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_fallback "a side missing a managed variable is refused" "is not assigned"

# --- the wiring, end to end, on the real file ------------------------------
#
# The fixtures above assert the merge logic against a file this suite writes.
# This asserts it against mk/gate-lists.mk itself, routed through .gitattributes
# by a real rebase, making the edit every gate-adding PR makes. It is the case
# that was failing in the repo while every assertion above passed: a fixture
# cannot be wrong about the file's shape when it is the file.

# append_entry FILE VAR ENTRY — add ENTRY as a new continuation line at the end
# of VAR's assignment. Two branches doing this land on adjacent lines, which is
# the conflict the driver exists to absorb.
append_entry() {
	awk -v v="$2" -v e="$3" '
		!inblock && $0 ~ "^"v"[ \t]*[:+?]?=" { inblock = 1 }
		inblock {
			if ($0 ~ /\\[ \t]*$/) { print; next }
			sub(/[ \t]*$/, "")
			print $0 " \\"
			print "                 " e
			inblock = 0
			appended = 1
			next
		}
		{ print }
		END {
			if (!appended) {
				printf "append_entry: %s not found\n", v > "/dev/stderr"
				exit 1
			}
		}
	' "$1"
}

REPO="$FIXTURE_DIR/e2e"
mkdir -p "$REPO/mk"
setup_rc=0
(
	cd "$REPO"
	git init -q -b main
	# Q820: no detached maintenance racing the next command in a fixture repo.
	git config maintenance.auto false
	git config user.email t@example.com
	git config user.name Test
	git config "merge.gatelists.name" 'test'
	git config "merge.gatelists.driver" "$DRIVER %O %A %B %L %P %S %X %Y"
	printf 'mk/gate-lists.mk merge=gatelists\n' >.gitattributes
	cp "$GATE_LISTS" mk/gate-lists.mk
	git add -A
	git commit -qm base
	git checkout -q -b topic
	append_entry mk/gate-lists.mk "$SUBJECT_VAR" q915-topic-test >mk/next
	mv mk/next mk/gate-lists.mk
	git commit -qam "topic appends"
	git checkout -q main
	append_entry mk/gate-lists.mk "$SUBJECT_VAR" q915-main-test >mk/next
	mv mk/next mk/gate-lists.mk
	git commit -qam "main appends"
	git checkout -q topic
) >"$FIXTURE_DIR/e2e-setup.log" 2>&1 || setup_rc=$?

E2E_DESC="a real rebase resolves adjacent appends to mk/gate-lists.mk"
if ((setup_rc != 0)); then
	bad "$E2E_DESC" "the fixture repo did not build: $(tail -2 "$FIXTURE_DIR/e2e-setup.log" | tr '\n' ' ')"
else
	set +e
	(cd "$REPO" && git rebase main) >"$FIXTURE_DIR/rebase.log" 2>&1
	rebase_rc=$?
	set -e
	if ((rebase_rc != 0)); then
		bad "$E2E_DESC" \
			"rc=$rebase_rc; $(grep -m1 'merge-gate-lists:' "$FIXTURE_DIR/rebase.log" || tail -2 "$FIXTURE_DIR/rebase.log" | tr '\n' ' ')"
	else
		# Nothing dropped and nothing invented: the file's own entry set plus the
		# one entry each side added. A driver that resolved by keeping one side
		# would still contain both new names.
		got="$(entries_of "$REPO/mk/gate-lists.mk" "$SUBJECT_VAR")"
		if [[ -s "$FIXTURE_DIR/make.err" ]]; then
			bad "$E2E_DESC" "make could not parse the merged file: $(head -1 "$FIXTURE_DIR/make.err")"
		else
			want="$( { entries_of "$GATE_LISTS" "$SUBJECT_VAR"; printf 'q915-main-test\nq915-topic-test\n'; } | sort)"
			if [[ "$got" == "$want" ]]; then
				ok "$E2E_DESC"
			else
				bad "$E2E_DESC" \
					"entry set differs: $(diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | head -6 | tr '\n' ' ')"
			fi
		fi
	fi
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
	echo "git-merge-gate-lists-test: ${fails} failure(s)"
	exit 1
fi
echo "git-merge-gate-lists-test: all assertions passed"
