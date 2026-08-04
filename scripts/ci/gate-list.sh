#!/usr/bin/env bash
#
# gate-list.sh - Render, or reconcile, the list of gates `make check` runs.
#
# The list used to exist in three hand-kept copies: a bulk `.PHONY` block at the
# top of the Makefile, the prose enumeration in `check:`'s own `##` help, and a
# second prose enumeration in docs/development/testing.md. Every gate-adding PR
# edited all three and concurrent branches conflicted on all three, while the
# copies still drifted — testing.md never named license-header-check or
# conflict-markers-check, both of which `make check` had been running (Q649).
#
# The source of truth is now CHECK_FAST_GATES + CHECK_HEAVY_GATES in the root
# Makefile, which the caller passes in already expanded by make. `--list` renders
# them with each gate's own `##` description so the docs can name a target
# instead of transcribing a list. `--check` is the drift gate: a derived list
# that can go stale silently is worse than three honest copies, so this asserts
# every consumer still agrees with those two variables.
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
#   5. testing.md cites `make list-gates`. This keeps the doc's one correct
#      reference alive; it cannot detect a transcribed list added *beside* the
#      pointer, which stays a review concern.
#
# Usage:
#   gate-list.sh --list  --fast '<names>' --heavy '<names>'
#   gate-list.sh --check --fast '<names>' --heavy '<names>' --status '<names>'
# Options (for the test suite; both default to the real files):
#   --makefile PATH   the Makefile to parse
#   --doc PATH        the doc that must cite `make list-gates`
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="Makefile"
DOC="docs/development/testing.md"
MODE=""
FAST=""
HEAVY=""
STATUS=""

while (($# > 0)); do
	case "$1" in
	--list | --check)
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
	--makefile)
		MAKEFILE="$2"
		shift
		;;
	--doc)
		DOC="$2"
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
	printf 'gate-list.sh: one of --list or --check is required\n' >&2
	exit 2
fi
if [[ -z "$FAST" || -z "$HEAVY" ]]; then
	printf 'gate-list.sh: --fast and --heavy are required\n' >&2
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

# 5. The doc names the target instead of transcribing the list.
if ! grep -qF 'make list-gates' "$DOC"; then
	fail "$DOC does not cite \`make list-gates\`; the gate list must be named, not transcribed"
fi

if ((fails > 0)); then
	printf '\n%d gate-list check(s) failed. The source of truth is CHECK_FAST_GATES +\n' "$fails" >&2
	printf 'CHECK_HEAVY_GATES in the Makefile; every consumer derives from those.\n' >&2
	exit 1
fi

printf 'gate lists agree: %s fast + %s heavy gates, one .PHONY each, docs point at make list-gates\n' \
	"$(wc -w <<<"$FAST" | tr -d ' ')" "$(wc -w <<<"$HEAVY" | tr -d ' ')"
