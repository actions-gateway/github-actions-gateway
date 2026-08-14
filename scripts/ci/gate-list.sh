#!/usr/bin/env bash
#
# gate-list.sh - Render, or reconcile, the lists `make check` derives from: the
# gates it runs, and the scripts/ suites its `scripts-test` gate fans out over.
#
# The gate list used to exist in three hand-kept copies: a bulk `.PHONY` block at
# the top of the Makefile, the prose enumeration in `check:`'s own `##` help, and
# a second prose enumeration in docs/development/testing.md. Every gate-adding PR
# edited all three and concurrent branches conflicted on all three, while the
# copies still drifted — testing.md never named license-header-check or
# conflict-markers-check, both of which `make check` had been running (Q649).
# SCRIPTS_TESTS carried the same defect in the same file: a `##` help line naming
# every suite in prose, so two suite-adding PRs conflicted by construction, and
# it had already drifted to 50 names for 55 suites (Q671).
#
# The source of truth is now CHECK_FAST_GATES + CHECK_HEAVY_GATES (gates) and
# SCRIPTS_TESTS (suites) in the root Makefile, which the caller passes in already
# expanded by make. `--list` renders the gates with each one's own `##`
# description and `--list-suites` renders the suites, so the docs and the help
# text can name a target instead of transcribing a list. `--check` is the drift
# gate: a derived list that can go stale silently is worse than honest copies, so
# this asserts every consumer still agrees with those variables.
#
# What --check asserts, and what it deliberately does not:
#   1. Every gate is declared .PHONY exactly once and carries a `##` help line —
#      otherwise `--list` prints a gate with no description, or make treats a
#      gate name as a file target.
#   2. `check:`'s recipe runs exactly CHECK_HEAVY_GATES, in order, as its
#      per-line $(MAKE) invocations, and its fan-out line expands
#      CHECK_FAST_GATES and nothing else. This is what keeps `--list` complete:
#      a gate wired straight into the recipe would run without being listed.
#   3. No target is declared .PHONY twice — the bulk block cannot come back.
#   4. STATUS_GATES is a subset of CHECK_FAST_GATES, the claim its comment makes.
#   5. testing.md cites `make list-gates` and `make list-script-tests`. This keeps
#      the doc's correct references alive; it cannot detect a transcribed list
#      added *beside* the pointer, which stays a review concern.
#   6. SCRIPTS_TESTS and the scripts/**/*-test.sh files on disk name the same
#      set, modulo NON_SUITE_TESTS below. A suite on disk but not in the variable
#      never runs — a disarmed gate that reports green — and one in the variable
#      but not on disk fails the fan-out on a missing file.
#   7. STATUS_GATES is complete, not only a subset: no fast gate outside it
#      selects docs/STATUS.md. Rule 4 alone let em-dash-check and
#      page-density-check scan the file from the day each was written while the
#      list called itself complete, so `make status-gates` reported a green
#      `make check` would not (Q749). The derivation is the pathspec git is
#      handed, the same question the gate itself asks. A gate that runs no
#      scripts/ file has no derivable file set and declares instead, with a
#      `# status-scope: none` comment directly above its .PHONY.
#   8. Every gate also runs in CI. A gate can join CHECK_FAST_GATES and be run
#      by no workflow, and rules 1-7 all stay green: `make check` enforces it
#      locally while every PR merges without it. comparison-stamps-check
#      shipped that way (#1440), and by 2026-08-13 five gates were in that
#      state — license-header-check, page-density-check,
#      semver-floor-sources-check, md-reflow-check and promql-check (Q831).
#      A gate is covered when a workflow runs its own `make` target, or when
#      every scripts/ file its recipe runs is run by CI some other way: through
#      another make target a workflow invokes (manifest-validate runs the three
#      chart-*-check scripts) or invoked directly (status-lint runs
#      lint-backlog.sh without make). A gate that is deliberately local-only
#      declares `# ci-scope: none` with its reason directly above its .PHONY,
#      the same shape rule 7 uses.
#
# Usage:
#   gate-list.sh --list        --fast '<names>' --heavy '<names>'
#   gate-list.sh --list-suites --suites '<paths>'
#   gate-list.sh --check --fast '<names>' --heavy '<names>' --status '<names>' \
#                        --suites '<paths>'
# Options (for the test suite; all default to the real files):
#   --makefile PATH    the Makefile to parse
#   --doc PATH         the doc that must cite the list targets
#   --scripts-dir PATH the tree scanned for *-test.sh files
#   --workflows PATH   the workflow tree rule 8 reads
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="Makefile"
DOC="docs/development/testing.md"
SCRIPTS_DIR="scripts"
STATUS_FILE="docs/STATUS.md"
WORKFLOWS_DIR=".github/workflows"
MODE=""
FAST=""
HEAVY=""
STATUS=""
SUITES=""

# scripts/**/*-test.sh files that are deliberately not `make scripts-test`
# suites, keyed by the same group/name form SCRIPTS_TESTS uses. Both are named
# for what they do, not for what they assert; adding one is rare and deliberate,
# which is why they are declared here rather than pattern-matched away.
NON_SUITE_TESTS=(
	agent/claude-usage-test # the Python suite runner, its own `make claude-usage-test` gate
	go/go-test              # the workspace `go test` runner behind `make test`
)

while (($# > 0)); do
	case "$1" in
	--list | --list-suites | --check)
		MODE="${1#--}"
		;;
	--fast)
		FAST="$2"
		shift
		;;
	--heavy)
		HEAVY="$2"
		shift
		;;
	--status)
		STATUS="$2"
		shift
		;;
	--suites)
		SUITES="$2"
		shift
		;;
	--makefile)
		MAKEFILE="$2"
		shift
		;;
	--doc)
		DOC="$2"
		shift
		;;
	--scripts-dir)
		SCRIPTS_DIR="$2"
		shift
		;;
	--workflows)
		WORKFLOWS_DIR="$2"
		shift
		;;
	*)
		printf 'gate-list.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

if [[ -z "$MODE" ]]; then
	printf 'gate-list.sh: one of --list, --list-suites or --check is required\n' >&2
	exit 2
fi
if [[ "$MODE" == "list-suites" ]]; then
	if [[ -z "$SUITES" ]]; then
		printf 'gate-list.sh: --list-suites requires --suites\n' >&2
		exit 2
	fi
elif [[ -z "$FAST" || -z "$HEAVY" ]]; then
	printf 'gate-list.sh: --fast and --heavy are required\n' >&2
	exit 2
fi
if [[ "$MODE" == "check" && -z "$SUITES" ]]; then
	printf 'gate-list.sh: --check requires --suites\n' >&2
	exit 2
fi
# Rule 8 reads this tree, and an empty read is indistinguishable from a tree
# where nothing is wired — it would report every gate as unwired. Refuse instead.
if [[ "$MODE" == "check" && ! -d "$WORKFLOWS_DIR" ]]; then
	printf 'gate-list.sh: --workflows %s is not a directory\n' "$WORKFLOWS_DIR" >&2
	exit 2
fi

# Backslash continuations are the Makefile's normal shape for these lists, so
# every parse below reads the joined form — a .PHONY or a recipe split across
# lines must read as one record or the assertions see half of it.
joined_makefile() {
	awk '{
		while (sub(/\\$/, "")) {
			if ((getline nxt) <= 0) break
			$0 = $0 nxt
		}
		print
	}' "$MAKEFILE"
}

# Every name declared phony, one per line, duplicates included — rule 3 counts
# them, so this must not deduplicate.
phony_names() {
	joined_makefile | awk '
		/^\.PHONY:/ {
			sub(/^\.PHONY:/, "")
			for (i = 1; i <= NF; i++) print $i
		}
	'
}

# The `##` help text of a target, empty when the target has none.
gate_help() {
	awk -v target="$1" '
		$0 ~ "^" target ":" {
			i = index($0, "##")
			if (i > 0) {
				desc = substr($0, i + 2)
				sub(/^[ \t]+/, "", desc)
				print desc
				exit
			}
		}
	' "$MAKEFILE"
}

# The recipe lines of the `check:` rule — tab-indented lines up to the first
# line that is not part of the recipe. Reads to EOF rather than exiting at the
# rule's end: exiting early would SIGPIPE the joined_makefile awk upstream, and
# pipefail turns that into a 141 the caller reads as a gate failure.
check_recipe() {
	joined_makefile | awk '
		/^check:/ { in_rule = 1; next }
		in_rule && $0 !~ /^\t/ { in_rule = 0 }
		in_rule { print }
	'
}

# Every *-test.sh under SCRIPTS_DIR, in SCRIPTS_TESTS's group/name form. A plain
# find rather than the git-backed select_present_files the content-scanning gates
# share: the question here is what exists to be run, so a suite written but not
# yet committed must count — that is exactly the state this rule catches.
disk_suites() {
	local path
	while IFS= read -r path; do
		path="${path#"$SCRIPTS_DIR"/}"
		printf '%s\n' "${path%.sh}"
	done < <(find "$SCRIPTS_DIR" -type f -name '*-test.sh') | sort
}

# The scripts/ files a gate's recipe runs, resolved under SCRIPTS_DIR so a
# fixture tree can stand in for the real one. Rule 8 passes `scripts` instead:
# what it matches against is workflow text, which spells the real path.
gate_scripts() {
	joined_makefile | awk -v target="$1" -v dir="${2-$SCRIPTS_DIR}" '
		$0 ~ "^" target ":" { in_rule = 1; next }
		in_rule && $0 !~ /^\t/ { in_rule = 0 }
		in_rule {
			rest = $0
			while (match(rest, /scripts\/[A-Za-z0-9_.\/-]+\.sh/)) {
				path = substr(rest, RSTART, RLENGTH)
				sub(/^scripts/, dir, path)
				print path
				rest = substr(rest, RSTART + RLENGTH)
			}
		}
	'
}

# The pathspecs a script selects files with. Only what it hands git counts: a
# gate reads the file set git returns, so a path named anywhere else — a message,
# a comment, a fixture — is not scope. \047 is the single quote.
selection_pathspecs() {
	awk '
		{
			line = $0
			sub(/^[ \t]*#.*$/, "", line)
			if (line !~ /git_candidates|git ls-files/) next
			if (line ~ /(git_candidates|git ls-files)[ \t]+\.([ \t]|$)/) print "."
			rest = line
			while (match(rest, /\047[^\047]*\047/)) {
				spec = substr(rest, RSTART + 1, RLENGTH - 2)
				rest = substr(rest, RSTART + RLENGTH)
				if (spec != "" && substr(spec, 1, 1) != ":") print spec
			}
		}
	' "$1"
}

# Whether a pathspec selects the backlog file. Command substitution rather than a
# pipe into `grep -q`: grep exits on the first match, and the SIGPIPE that sends
# upstream becomes a 141 under pipefail that reads as "not selected".
selects_status_file() {
	local listed
	listed="$(git ls-files -- "$1")" || return 1
	grep -qx -- "$STATUS_FILE" <<<"$listed"
}

# Whether the comment block directly above a target's .PHONY declares it out of
# KEY's scope (`status-scope` for rule 7, `ci-scope` for rule 8). Adjacency is
# the point: the reason belongs at the declaration.
scope_none() {
	joined_makefile | awk -v target="$1" -v key="$2" '
		/^#/ { if ($0 ~ key ":[ \t]*none") marked = 1; next }
		/^\.PHONY:/ {
			for (i = 2; i <= NF; i++) if ($i == target && marked) hit = 1
		}
		{ marked = 0 }
		END { exit(hit ? 0 : 1) }
	'
}

# The workflow tree with its comment lines blanked. Dropping them is what keeps
# rule 8 honest: these files explain themselves in prose that names the very
# targets and scripts the rule looks for, and a gate named in a comment gates
# nothing. Anchoring to command position is the same discipline the go-throttle
# and foreground-guard hooks apply to their own registries.
workflow_text() {
	{ cat "$WORKFLOWS_DIR"/*.yml 2>/dev/null || true; } | awk '{ sub(/^[ \t]*#.*$/, ""); print }'
}

# The make targets the workflows invoke, one per line.
ci_make_targets() {
	{ grep -oE '\bmake +(-[A-Za-z-]+ +)*[a-z0-9][a-z0-9-]*' <<<"$(workflow_text)" || true; } |
		awk '{ print $NF }' | sort -u
}

# The scripts/ files CI runs by way of a make target it invokes.
ci_target_scripts() {
	local target
	while IFS= read -r target; do
		[[ -n "$target" ]] || continue
		gate_scripts "$target" scripts
	done < <(ci_make_targets) | sort -u
}

if [[ "$MODE" == "list-suites" ]]; then
	suite_n=$(wc -w <<<"$SUITES" | tr -d ' ')
	printf 'make scripts-test runs %d scripts/ suites, concurrently.\n' "$suite_n"
	printf 'Each is named for the script it asserts; run one on its own with the path below.\n'

	group=""
	while IFS= read -r suite; do
		if [[ "${suite%%/*}" != "$group" ]]; then
			group="${suite%%/*}"
			printf '\n  %s/\n' "$group"
		fi
		printf '    %s/%s.sh\n' "$SCRIPTS_DIR" "$suite"
	done < <(tr ' ' '\n' <<<"$SUITES" | grep -v '^$' | sort)
	exit 0
fi

if [[ "$MODE" == "list" ]]; then
	fast_n=$(wc -w <<<"$FAST" | tr -d ' ')
	heavy_n=$(wc -w <<<"$HEAVY" | tr -d ' ')
	printf 'make check runs %d gates, in this order.\n' "$((fast_n + heavy_n))"
	printf "Each description is that target's own ## help line, clipped; make help prints it in full.\n\n"

	render() {
		local gate desc
		for gate in $1; do
			desc="$(gate_help "$gate")"
			if ((${#desc} > 96)); then
				desc="${desc:0:95}…"
			fi
			printf '  %-24s %s\n' "$gate" "$desc"
		done
	}

	printf 'Fast gates (%d) — run concurrently, none takes a heavy-build slot:\n' "$fast_n"
	render "$FAST"
	printf '\nHeavy gates (%d) — run sequentially, each takes a machine-wide build slot:\n' "$heavy_n"
	render "$HEAVY"
	exit 0
fi

fails=0
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

all_phony="$(phony_names)"

# 1. Every gate is a documented .PHONY target.
for gate in $FAST $HEAVY; do
	count=$(grep -cx -- "$gate" <<<"$all_phony" || true)
	if ((count == 0)); then
		fail "gate '$gate' runs in \`make check\` but no .PHONY declares it"
	fi
	if [[ -z "$(gate_help "$gate")" ]]; then
		fail "gate '$gate' has no \`##\` help line, so \`make list-gates\` would print it blank"
	fi
done

# 2. The recipe matches the variables. The fan-out line must expand
# CHECK_FAST_GATES once and contain exactly one $(MAKE) (the one inside the
# foreach); every other recipe line contributes its $(MAKE) target to the heavy
# sequence. A gate wired straight into the recipe therefore lands in one bucket
# or the other and fails the comparison.
recipe="$(check_recipe)"
fanout="$(grep -F 'run-parallel.sh' <<<"$recipe" || true)"
# shellcheck disable=SC2016 # the grep patterns are make source text, not shell expansions
if [[ -z "$fanout" ]]; then
	fail "\`check:\` no longer fans out through run-parallel.sh; the fast-gate list is unverifiable"
elif ! grep -qF '$(CHECK_FAST_GATES)' <<<"$fanout"; then
	fail "\`check:\`'s run-parallel line does not expand \$(CHECK_FAST_GATES)"
else
	make_refs=$(grep -oF '$(MAKE)' <<<"$fanout" | wc -l | tr -d ' ')
	if ((make_refs != 1)); then
		fail "\`check:\`'s run-parallel line has $make_refs \$(MAKE) references, want 1 — a gate is wired in by hand instead of via CHECK_FAST_GATES"
	fi
fi

recipe_heavy="$(grep -vF 'run-parallel.sh' <<<"$recipe" | awk '
	{ for (i = 1; i <= NF; i++) if ($i == "$(MAKE)") print $(i + 1) }
')"
want_heavy="$(tr ' ' '\n' <<<"$HEAVY" | grep -v '^$' || true)"
if [[ "$recipe_heavy" != "$want_heavy" ]]; then
	fail "\`check:\`'s sequential phases do not match CHECK_HEAVY_GATES
       recipe runs: $(tr '\n' ' ' <<<"$recipe_heavy")
       CHECK_HEAVY_GATES: $HEAVY"
fi

# 3. No target declared .PHONY twice — that is how the bulk block returns.
dupes="$(sort <<<"$all_phony" | uniq -d)"
if [[ -n "$dupes" ]]; then
	fail "targets declared .PHONY more than once (declare each once, beside its rule): $(tr '\n' ' ' <<<"$dupes")"
fi

# 4. STATUS_GATES is the strict subset its comment claims.
for gate in $STATUS; do
	if ! grep -qw -- "$gate" <<<"$FAST"; then
		fail "STATUS_GATES member '$gate' is not in CHECK_FAST_GATES, so \`make status-gates\` is a second opinion rather than a subset"
	fi
done

# 5. The doc names the targets instead of transcribing the lists.
for target in list-gates list-script-tests; do
	if ! grep -qF "make $target" "$DOC"; then
		fail "$DOC does not cite \`make $target\`; the list must be named, not transcribed"
	fi
done

# 6. SCRIPTS_TESTS and the *-test.sh files on disk are the same set.
want_suites="$(tr ' ' '\n' <<<"$SUITES" | grep -v '^$' | sort)"
have_suites="$(disk_suites)"
for suite in "${NON_SUITE_TESTS[@]}"; do
	if grep -qx -- "$suite" <<<"$want_suites"; then
		fail "'$suite' is in SCRIPTS_TESTS and in gate-list.sh's NON_SUITE_TESTS; it cannot be both"
	fi
	have_suites="$(grep -vx -- "$suite" <<<"$have_suites" || true)"
done

unlisted="$(comm -13 <(printf '%s\n' "$want_suites") <(printf '%s\n' "$have_suites"))"
if [[ -n "$unlisted" ]]; then
	fail "these $SCRIPTS_DIR/**/*-test.sh suites are not in SCRIPTS_TESTS, so \`make scripts-test\` never runs them: $(tr '\n' ' ' <<<"$unlisted")
       add each to SCRIPTS_TESTS, or to NON_SUITE_TESTS in this script if it is not a suite"
fi
missing="$(comm -23 <(printf '%s\n' "$want_suites") <(printf '%s\n' "$have_suites"))"
if [[ -n "$missing" ]]; then
	fail "these SCRIPTS_TESTS entries have no $SCRIPTS_DIR/<name>.sh on disk: $(tr '\n' ' ' <<<"$missing")"
fi

# 7. The other direction on STATUS_GATES: a fast gate left out of it must be one
# a $STATUS_FILE-only change cannot fail. Rule 4 could only see the members.
status_names="$(tr ' ' '\n' <<<"$STATUS" | grep -v '^$' || true)"
for gate in $FAST; do
	if grep -qx -- "$gate" <<<"$status_names"; then
		continue
	fi
	scanned=0
	flagged=0
	while IFS= read -r script; do
		[[ -f "$script" ]] || continue
		scanned=1
		while IFS= read -r spec; do
			[[ -n "$spec" ]] || continue
			((flagged == 0)) || continue
			if selects_status_file "$spec"; then
				flagged=1
				fail "gate '$gate' selects $STATUS_FILE (pathspec '$spec' in $script) but STATUS_GATES omits it, so \`make status-gates\` reports a green \`make check\` would not
       add it to STATUS_GATES, or narrow the pathspec"
			fi
		done < <(selection_pathspecs "$script")
	done < <(gate_scripts "$gate")
	if ((scanned == 0)) && ! scope_none "$gate" status-scope; then
		fail "gate '$gate' runs no $SCRIPTS_DIR/ file, so whether a $STATUS_FILE-only change can fail it is not derivable
       add it to STATUS_GATES, or declare \`# status-scope: none\` with the reason directly above its .PHONY"
	fi
done

# 8. Every gate also runs in CI, so `make check` is not the only thing enforcing
# it. A gate nobody wired gates nothing on a PR while every rule above stays
# green — the failure reports as a clean gate list (Q831).
ci_targets="$(ci_make_targets)"
ci_scripts="$(ci_target_scripts)"
ci_text="$(workflow_text)"
for gate in $FAST $HEAVY; do
	if grep -qx -- "$gate" <<<"$ci_targets"; then
		continue
	fi
	if scope_none "$gate" ci-scope; then
		continue
	fi
	uncovered=""
	scanned=0
	while IFS= read -r script; do
		[[ -n "$script" ]] || continue
		scanned=1
		# Run by another make target a workflow invokes, or invoked directly.
		if grep -qxF -- "$script" <<<"$ci_scripts" || grep -qF -- "$script" <<<"$ci_text"; then
			continue
		fi
		uncovered="$uncovered $script"
	done < <(gate_scripts "$gate" scripts)
	if ((scanned == 0)); then
		fail "gate '$gate' runs in \`make check\` but no workflow runs \`make $gate\`, and its recipe runs no scripts/ file to cover it another way — so it gates nothing on a PR
       run it from a workflow, or declare \`# ci-scope: none\` with the reason directly above its .PHONY"
	elif [[ -n "$uncovered" ]]; then
		fail "gate '$gate' runs in \`make check\` but not in CI: no workflow runs \`make $gate\`, and CI runs none of$uncovered
       run it from a workflow, or declare \`# ci-scope: none\` with the reason directly above its .PHONY"
	fi
done

if ((fails > 0)); then
	printf '\n%d gate-list check(s) failed. The source of truth is CHECK_FAST_GATES +\n' "$fails" >&2
	printf 'CHECK_HEAVY_GATES and SCRIPTS_TESTS in the Makefile; every consumer derives\n' >&2
	printf 'from those.\n' >&2
	exit 1
fi

printf 'gate lists agree: %s fast + %s heavy gates, one .PHONY each; %s scripts/ suites match the tree; docs point at the list targets\n' \
	"$(wc -w <<<"$FAST" | tr -d ' ')" "$(wc -w <<<"$HEAVY" | tr -d ' ')" "$(wc -w <<<"$SUITES" | tr -d ' ')"
