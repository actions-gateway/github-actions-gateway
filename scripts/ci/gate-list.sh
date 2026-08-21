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
#   4. QUEUE_GATES is a subset of CHECK_FAST_GATES, the claim its comment makes.
#   5. testing.md cites `make list-gates` and `make list-script-tests`. This keeps
#      the doc's correct references alive; it cannot detect a transcribed list
#      added *beside* the pointer, which stays a review concern.
#   6. SCRIPTS_TESTS and the scripts/**/*-test.sh files on disk name the same
#      set, modulo NON_SUITE_TESTS below. A suite on disk but not in the variable
#      never runs — a disarmed gate that reports green — and one in the variable
#      but not on disk fails the fan-out on a missing file.
#   7. QUEUE_GATES is complete, not only a subset: no fast gate outside it
#      selects a backlog item. Rule 4 alone let em-dash-check and
#      page-density-check scan the file from the day each was written while the
#      list called itself complete, so `make queue-gates` reported a green
#      `make check` would not (Q749). The derivation is the pathspec git is
#      handed, the same question the gate itself asks. A gate that runs no
#      scripts/ file has no derivable file set and declares instead, with a
#      `# queue-scope: none` comment directly above its .PHONY.
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
#      check-queue-rules.sh without make). A gate that is deliberately local-only
#      declares `# ci-scope: none` with its reason directly above its .PHONY,
#      the same shape rule 7 uses.
#   9. DOCS_GATES is a subset of CHECK_FAST_GATES, the claim its comment makes.
#      Rule 4, one list over — and the list nothing was passing in:
#      release-notes-check sat in DOCS_GATES and in neither gate list, so
#      `make docs-gates` ran a gate `make check` did not (Q920).
#  10. DOCS_GATES is complete, not only a subset: no fast gate outside it is one
#      a change to a page under docs/ can fail. Rule 7's derivation one list
#      over, reading the same pathspecs, and it inherits rule 7's blind spot —
#      a gate that hardcodes the page it reads hands git nothing, so this
#      cannot see it. Page-scoped docs gates are written that way, which is why
#      the blind spot costs more here than it does on rule 7 (Q930).
#  11. The workflow that covers a gate is one the merge queue evaluates. Rule 8
#      asks whether some workflow runs the gate and stops there, so a gate whose
#      only workflow triggers on `pull_request` alone satisfies it while never
#      running on the queue's candidate merge — and the candidate is the only
#      commit that carries the merge result (Q942). The tree-visible half of
#      that question is the trigger: a workflow with no `merge_group` in its
#      `on:` block provably never reports there. Whether the check it does
#      report is *required*, and so blocking, is a repo-settings question this
#      cannot read (Q943). A gate deliberately kept off the candidate merge
#      declares `# merge-queue-scope: none`, the shape rules 7, 8 and 10 use.
#
# Usage:
#   gate-list.sh --list        --fast '<names>' --heavy '<names>'
#   gate-list.sh --list-suites --suites '<paths>'
#   gate-list.sh --check --fast '<names>' --heavy '<names>' --queue '<names>' \
#                        --docs '<names>' --suites '<paths>'
# Options (for the test suite; all default to the real files):
#   --makefile PATH    the Makefile to parse
#   --doc PATH         the doc that must cite the list targets
#   --scripts-dir PATH the tree scanned for *-test.sh files
#   --workflows PATH   the workflow tree rules 8 and 11 read
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="Makefile"
DOC="docs/development/testing.md"
SCRIPTS_DIR="scripts"
# A backlog edit is an edit to one item file. Which item does not matter --
# the question rule 7 asks is whether a gate's pathspec reaches into the store
# -- so the first tracked item stands for all of them, and the answer stays
# correct as items are filed and closed. Resolved once, here, rather than per
# pathspec: `git ls-files` over the whole store is the expensive half.
# The glob is a variable, not a literal on the `git ls-files` line, and that is
# load-bearing: selection_pathspecs below scans a gate's scripts for the literal
# pathspecs they hand git, and this script IS the script gate-lists-check runs.
# A literal here would report this gate as one a backlog edit can fail, which it
# cannot -- it reads one filename to answer rule 7 and checks nothing in it.
QUEUE_GLOB='docs/queue/Q*.md'
QUEUE_FILE="$(git ls-files -- "$QUEUE_GLOB" | sort | head -1)"
WORKFLOWS_DIR=".github/workflows"
MODE=""
FAST=""
HEAVY=""
QUEUE=""
DOCS=""
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
	--queue)
		QUEUE="$2"
		shift
		;;
	--docs)
		DOCS="$2"
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
# An empty DOCS_GATES is the shape rule 9 cannot report: a renamed or mistyped
# variable expands to nothing, and a subset assertion over no members passes.
# Rule 10 would still go red, but only while some fast gate happens to hand git
# a docs/ pathspec — a property of the tree, not an invariant. Refuse instead.
if [[ "$MODE" == "check" && -z "$DOCS" ]]; then
	printf 'gate-list.sh: --check requires a non-empty --docs\n' >&2
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

# Whether a pathspec selects a backlog item. Command substitution rather than a
# pipe into `grep -q`: grep exits on the first match, and the SIGPIPE that sends
# upstream becomes a 141 under pipefail that reads as "not selected".
selects_queue_file() {
	local listed
	[[ -n "$QUEUE_FILE" ]] || return 1
	listed="$(git ls-files -- "$1")" || return 1
	grep -qx -- "$QUEUE_FILE" <<<"$listed"
}

# Whether a pathspec selects a page under docs/. Any tracked Markdown there
# answers rule 10: unlike the queue store, docs/ is not homogeneous, so no
# single file stands for the tree the way QUEUE_FILE stands for an item.
# Command substitution rather than a pipe, for the reason above.
selects_docs_page() {
	local listed
	listed="$(git ls-files -- "$1")" || return 1
	grep -qE '^docs/.+\.md$' <<<"$listed"
}

# Whether the comment block directly above a target's .PHONY declares it out of
# KEY's scope (`queue-scope` for rule 7, `ci-scope` for rule 8). Adjacency is
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

# The named workflow files with their comment lines blanked. Dropping them is
# what keeps rule 8 honest: these files explain themselves in prose that names
# the very targets and scripts the rule looks for, and a gate named in a comment
# gates nothing. Anchoring to command position is the same discipline the
# go-throttle and foreground-guard hooks apply to their own registries.
# Files rather than the tree, because rule 11 asks the same questions of the
# merge-queue subset -- and an empty subset must read as no coverage rather than
# as `cat` waiting on stdin.
workflow_text() {
	(($# > 0)) || return 0
	{ cat "$@" 2>/dev/null || true; } | awk '{ sub(/^[ \t]*#.*$/, ""); print }'
}

# The make targets a body of workflow text invokes, one per line.
ci_make_targets() {
	{ grep -oE '\bmake +(-[A-Za-z-]+ +)*[a-z0-9][a-z0-9-]*' <<<"$1" || true; } |
		awk '{ print $NF }' | sort -u
}

# The scripts/ files that text runs by way of a make target it invokes.
ci_target_scripts() {
	local target
	while IFS= read -r target; do
		[[ -n "$target" ]] || continue
		gate_scripts "$target" scripts
	done < <(ci_make_targets "$1") | sort -u
}

# The workflow files the merge queue evaluates: those whose `on:` block declares
# `merge_group`. Comments come off every line first, for the reason
# workflow_text drops them -- these files explain their own triggers in prose,
# and a trigger named in a comment fires nothing. A blanked line does not close
# the block, so a commented-out trigger inside it is skipped rather than read as
# the end of `on:`.
merge_queue_workflows() {
	local wf
	for wf in "$WORKFLOWS_DIR"/*.yml; do
		[[ -f "$wf" ]] || continue
		if awk '
			{ line = $0; sub(/[ \t]*#.*$/, "", line) }
			line ~ /^"?on"?:/ { in_on = 1; if (line ~ /merge_group/) found = 1; next }
			in_on && line != "" && line ~ /^[^ \t]/ { in_on = 0 }
			in_on && line ~ /^[ \t]+merge_group[ \t]*:/ { found = 1 }
			END { exit(found ? 0 : 1) }
		' "$wf"; then
			printf '%s\n' "$wf"
		fi
	done
}

# How a body of CI coverage reaches a gate: `target` when it runs `make <gate>`,
# `scripts` when it runs every scripts/ file the gate's recipe runs, `none` when
# that recipe runs no scripts/ file to derive an answer from, and
# `uncovered:<paths>` for the files it leaves out. Rules 8 and 11 ask this of
# two different file sets and differ only in what they do with the answer.
ci_coverage() {
	local gate="$1" targets="$2" scripts="$3" text="$4"
	local script uncovered="" scanned=0
	if grep -qx -- "$gate" <<<"$targets"; then
		printf 'target\n'
		return 0
	fi
	while IFS= read -r script; do
		[[ -n "$script" ]] || continue
		scanned=1
		# Run by another make target a workflow invokes, or invoked directly.
		if grep -qxF -- "$script" <<<"$scripts" || grep -qF -- "$script" <<<"$text"; then
			continue
		fi
		uncovered="$uncovered $script"
	done < <(gate_scripts "$gate" scripts)
	if ((scanned == 0)); then
		printf 'none\n'
	elif [[ -n "$uncovered" ]]; then
		printf 'uncovered:%s\n' "$uncovered"
	else
		printf 'scripts\n'
	fi
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

# 4. QUEUE_GATES is the strict subset its comment claims.
for gate in $QUEUE; do
	if ! grep -qw -- "$gate" <<<"$FAST"; then
		fail "QUEUE_GATES member '$gate' is not in CHECK_FAST_GATES, so \`make queue-gates\` is a second opinion rather than a subset"
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

# 7. The other direction on QUEUE_GATES: a fast gate left out of it must be one
# a backlog-only change cannot fail. Rule 4 could only see the members.
queue_names="$(tr ' ' '\n' <<<"$QUEUE" | grep -v '^$' || true)"
for gate in $FAST; do
	if grep -qx -- "$gate" <<<"$queue_names"; then
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
			if selects_queue_file "$spec"; then
				flagged=1
				fail "gate '$gate' selects $QUEUE_FILE (pathspec '$spec' in $script) but QUEUE_GATES omits it, so \`make queue-gates\` reports a green \`make check\` would not
       add it to QUEUE_GATES, or narrow the pathspec"
			fi
		done < <(selection_pathspecs "$script")
	done < <(gate_scripts "$gate")
	if ((scanned == 0)) && ! scope_none "$gate" queue-scope; then
		fail "gate '$gate' runs no $SCRIPTS_DIR/ file, so whether a backlog-only change can fail it is not derivable
       add it to QUEUE_GATES, or declare \`# queue-scope: none\` with the reason directly above its .PHONY"
	fi
done

# 8. Every gate also runs in CI, so `make check` is not the only thing enforcing
# it. A gate nobody wired gates nothing on a PR while every rule above stays
# green — the failure reports as a clean gate list (Q831).
ci_text="$(workflow_text "$WORKFLOWS_DIR"/*.yml)"
ci_targets="$(ci_make_targets "$ci_text")"
ci_scripts="$(ci_target_scripts "$ci_text")"
declare -A ci_verdict=()
for gate in $FAST $HEAVY; do
	if scope_none "$gate" ci-scope; then
		continue
	fi
	ci_verdict["$gate"]="$(ci_coverage "$gate" "$ci_targets" "$ci_scripts" "$ci_text")"
	case "${ci_verdict["$gate"]}" in
	none)
		fail "gate '$gate' runs in \`make check\` but no workflow runs \`make $gate\`, and its recipe runs no scripts/ file to cover it another way — so it gates nothing on a PR
       run it from a workflow, or declare \`# ci-scope: none\` with the reason directly above its .PHONY"
		;;
	uncovered:*)
		fail "gate '$gate' runs in \`make check\` but not in CI: no workflow runs \`make $gate\`, and CI runs none of${ci_verdict["$gate"]#uncovered:}
       run it from a workflow, or declare \`# ci-scope: none\` with the reason directly above its .PHONY"
		;;
	esac
done

# 9. DOCS_GATES is the strict subset its comment claims — rule 4, one list over.
for gate in $DOCS; do
	if ! grep -qw -- "$gate" <<<"$FAST"; then
		fail "DOCS_GATES member '$gate' is not in CHECK_FAST_GATES, so \`make docs-gates\` is a second opinion rather than a subset"
	fi
done

# 10. The other direction on DOCS_GATES, rule 7's derivation over docs/ rather
# than the backlog store: a fast gate left out of it must be one no change to a
# page under docs/ can fail.
docs_names="$(tr ' ' '\n' <<<"$DOCS" | grep -v '^$' || true)"
for gate in $FAST; do
	if grep -qx -- "$gate" <<<"$docs_names"; then
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
			if selects_docs_page "$spec"; then
				flagged=1
				fail "gate '$gate' selects pages under docs/ (pathspec '$spec' in $script) but DOCS_GATES omits it, so \`make docs-gates\` reports a green \`make check\` would not
       add it to DOCS_GATES, or narrow the pathspec"
			fi
		done < <(selection_pathspecs "$script")
	done < <(gate_scripts "$gate")
	if ((scanned == 0)) && ! scope_none "$gate" docs-scope; then
		fail "gate '$gate' runs no $SCRIPTS_DIR/ file, so whether a docs/ change can fail it is not derivable
       add it to DOCS_GATES, or declare \`# docs-scope: none\` with the reason directly above its .PHONY"
	fi
done

# 11. The workflow covering it is one the merge queue evaluates. Rule 8 stops at
# `some workflow runs it`, which a `pull_request`-only workflow satisfies while
# the queue's candidate merge — the one commit carrying the merge result — runs
# nothing (Q942). Only gates rule 8 passed are asked: a gate no workflow runs at
# all has one defect, not two.
mq_workflows=()
while IFS= read -r wf; do
	[[ -n "$wf" ]] || continue
	mq_workflows+=("$wf")
done < <(merge_queue_workflows)
mq_text="$(workflow_text "${mq_workflows[@]}")"
mq_targets="$(ci_make_targets "$mq_text")"
mq_scripts="$(ci_target_scripts "$mq_text")"
for gate in $FAST $HEAVY; do
	case "${ci_verdict["$gate"]-}" in
	target | scripts) ;;
	*) continue ;;
	esac
	if scope_none "$gate" merge-queue-scope; then
		continue
	fi
	case "$(ci_coverage "$gate" "$mq_targets" "$mq_scripts" "$mq_text")" in
	target | scripts) ;;
	*)
		fail "gate '$gate' runs in CI but in no workflow the merge queue evaluates: every workflow covering it triggers without \`merge_group\`, so the candidate merge is never held to it
       add \`merge_group:\` to that workflow's \`on:\` block, or declare \`# merge-queue-scope: none\` with the reason directly above its .PHONY"
		;;
	esac
done

if ((fails > 0)); then
	printf '\n%d gate-list check(s) failed. The source of truth is CHECK_FAST_GATES +\n' "$fails" >&2
	printf 'CHECK_HEAVY_GATES and SCRIPTS_TESTS in the Makefile; every consumer derives\n' >&2
	printf 'from those.\n' >&2
	exit 1
fi

printf 'gate lists agree: %s fast + %s heavy gates, one .PHONY each; %s scripts/ suites match the tree; docs point at the list targets\n' \
	"$(wc -w <<<"$FAST" | tr -d ' ')" "$(wc -w <<<"$HEAVY" | tr -d ' ')" "$(wc -w <<<"$SUITES" | tr -d ' ')"
