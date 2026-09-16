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
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
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
# source, so a reformatted managed list cannot make the reconciliation below
# read a list nobody runs on.
mapfile -t MANAGED < <("$DRIVER" --managed-vars)
if ((${#MANAGED[@]} == 0)); then
	echo "FAIL the driver reports the variables it manages"
	echo "     $DRIVER --managed-vars produced nothing"
	exit 1
fi

# The same rule mklists.Lift opens an assignment with, so the two agree on what
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
# name the driver manages that the file does not assign makes mklists.Lift
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
		"the driver names ${stale[*]}, which mk/gate-lists.mk does not assign; the driver refuses every merge of that file until the two agree"
fi

unmanaged=()
for v in "${ASSIGNED[@]}"; do
	contains "$v" "${MANAGED[@]}" || unmanaged+=("$v")
done
if ((${#unmanaged[@]} == 0)); then
	ok "every list in mk/gate-lists.mk is one the driver manages"
else
	bad "every list in mk/gate-lists.mk is one the driver manages" \
		"mk/gate-lists.mk assigns ${unmanaged[*]}, which the driver omits; appends to those still conflict by line position"
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

# makefile_wrapped ENTRIES — the same makefile with $SUBJECT_VAR written over
# continuations, which the re-render case needs and `makefile` cannot give it.
# The driver takes its wrap width from the assignment's head line, so a list
# written on one line yields a width wider than anything a re-render emits and
# nothing ever wraps. Only this shape reaches the wrapping.
makefile_wrapped() {
	local v e first
	local -a entries
	read -r -a entries <<<"$1"
	echo "# leading prose"
	for v in "${ASSIGNED[@]}"; do
		echo
		case "$v" in
		"$SUBJECT_VAR")
			first=1
			for e in "${entries[@]}"; do
				if ((first)); then
					printf '%s := %s' "$v" "$e"
					first=0
				else
					printf ' \\\n                 %s' "$e"
				fi
			done
			printf '\n'
			;;
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
	die_if_killed "$desc" "$LAST_RC"
	if ((LAST_RC != 0)); then
		bad "$desc" "driver reported a conflict: $(head -1 "$FIXTURE_DIR/err")"
		return
	fi
	local got want
	# Tolerate a failing read, because a failing read is the interesting case:
	# entries_of pipes make through tr/sed/sort, so under `set -o pipefail` a
	# makefile make cannot parse takes the whole suite down with it and the
	# check below never runs. That is the one failure this oracle exists to
	# catch -- a rendered block that dropped a continuation parses as a
	# fraction of its list -- so it has to reach the report rather than abort.
	got="$(entries_of "$FIXTURE_DIR/out" "$var")" || true
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

# --- the re-render path, which only a removal reaches ------------------------
#
# Every case above is an append or a refusal, so the driver's re-render branch
# runs in none of them: an append keeps ours' lines and rewraps nothing. A
# removal forces the whole block to be rebuilt, which is where a wrap can be
# emitted without its continuation and silently assign a fraction of the list.
#
# Both sides have to change the list, or the assertion cannot fail. With only
# theirs changing, a driver that renders a broken block falls back, and the
# plain three-way merge of a one-sided change is clean and produces the right
# answer — so the guard and the fallback agree and the test passes either way.
# Here ours adds and theirs removes, so the fallback conflicts and only a
# correct render can satisfy the assertion.
#
# The entries are long and the assignment is wrapped so the rebuilt block has
# to wrap too.
LONG="long-entry-aaaa long-entry-bbbb long-entry-cccc long-entry-dddd"
LONG="$LONG long-entry-eeee long-entry-ffff long-entry-gggg long-entry-hhhh"

makefile_wrapped "$LONG doomed-entry" >"$FIXTURE_DIR/base"
makefile_wrapped "$LONG doomed-entry ours-added" >"$FIXTURE_DIR/ours"
makefile_wrapped "$LONG" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
# shellcheck disable=SC2086 # $LONG is a deliberate word-split into arguments
expect_set "a removal re-renders the block and every line still continues" "$SUBJECT_VAR" \
	$LONG ours-added

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
die_if_killed "$E2E_DESC" "$setup_rc"
if ((setup_rc != 0)); then
	bad "$E2E_DESC" "the fixture repo did not build: $(tail -2 "$FIXTURE_DIR/e2e-setup.log" | tr '\n' ' ')"
else
	set +e
	(cd "$REPO" && git rebase main) >"$FIXTURE_DIR/rebase.log" 2>&1
	rebase_rc=$?
	set -e
	die_if_killed "$E2E_DESC" "$rebase_rc"
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
