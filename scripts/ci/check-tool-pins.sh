#!/usr/bin/env bash
#
# check-tool-pins.sh — assert every `.build/` tool rule declares the file that
# carries its version pin as a prerequisite (Q904).
#
# Q842 set the pattern and a Makefile comment states it: without such a
# prerequisite make treats an existing binary as up to date forever, so a bump
# serves the old binary against the new target. A stale mdreflow met an unknown
# --explain flag (loud), then reported four false reflow failures on a clean
# tree (silent). Only the comment asked for it, so cosign was added with no
# prerequisite at all and served a stale verifier until Q857 — the next non-Go
# pinned tool repeats it.
#
# check-cosign-pin.sh is not this gate. It holds two version *strings* to each
# other across two files (Q903) and never asks whether a rule fires on a bump.
#
# The database, not a regexp. `make -pnq` dumps the resolved rules, so variable
# expansion, backslash continuations and the eight-target rule the Go tools
# share are make's problem rather than a pattern's — the parser that stops
# matching is this gate's own failure mode, and it would report green.
#
# The database's shape is version-dependent in one place that matters. A
# target-specific variable ($(MDREFLOW): TOOL_PKG := ...) prints as a comment
# under GNU make 3.81, the make macOS ships, and as a rule-shaped
# `target: VAR := value` line under 4.x, which CI runs. Read naively the 4.x
# form makes `TOOL_PKG`, `:=` and the package path three prerequisites of
# .build/mdreflow; measured on run 32275796483, where every Go tool rule failed
# that way while the same tree passed locally. Assignment lines are dropped
# below, and parse_rules is exercised against a captured 4.x database so the
# shape this box cannot produce is still asserted.
#
# A tool binary is a `.build/` target that is not a prerequisite of another
# `.build/` target. That is what separates the tools from the pin sentinels
# ($(COSIGN_PIN)), which are prerequisites and correctly carry none themselves.
#
# Two assertions per tool, because the second is the one the first cannot make:
#   1. It declares at least one NORMAL prerequisite. Order-only prerequisites —
#      everything after the `|` — never make a target out of date when they are
#      newer, so a rule carrying only those has the Q857 defect while appearing
#      to have a prerequisite.
#   2. Every normal prerequisite either is tracked in git, a manifest that moves
#      on a bump (tools/go.mod, tools/vendor/modules.txt, cmd/gmc/go.sum), or is
#      version-keyed. Version-keyed is measured rather than pattern-matched: the
#      database is read a second time with every makefile `*_VERSION` variable
#      overridden, and the path must move. A `.build/trivy.stamp` that is
#      touched once and never again is the Q857 defect wearing a prerequisite,
#      and assertion 1 alone passes it.
#
# Usage:
#   check-tool-pins.sh [--makefile PATH]
#   check-tool-pins.sh --database PATH --print-rules
#
# --database reads a captured `make -p` dump instead of invoking make, and
# --print-rules prints the parsed `target|prereqs` lines. Together they are the
# test seam for the parser, which has to hold across make versions this machine
# cannot both run.
#
# Exits 1 on a finding, and 2 when the read yielded no `.build/` tool rules at
# all — a database this gate could not parse must not report every rule pinned.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

MAKEFILE="Makefile"
DATABASE=""
PRINT_RULES=""
# Overriding every version variable at once keeps the probe to one extra make
# run. The value only has to differ from the real pin; it is never built.
PROBE_VERSION="__tool_pin_probe__"

while (($# > 0)); do
	case "$1" in
	--makefile)
		MAKEFILE="$2"
		shift
		;;
	--database)
		DATABASE="$2"
		shift
		;;
	--print-rules)
		PRINT_RULES=1
		;;
	*)
		printf 'check-tool-pins.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

# The two are one seam, and neither half is a check on its own: --database with
# no --print-rules would parse a dump and exit 0 having asserted nothing, which
# is the shape every refusal in this script exists to prevent.
if [[ -n "$PRINT_RULES" && -z "$DATABASE" ]]; then
	printf 'check-tool-pins.sh: --print-rules requires --database\n' >&2
	exit 2
fi
if [[ -n "$DATABASE" && -z "$PRINT_RULES" ]]; then
	printf 'check-tool-pins.sh: --database is a parser seam and requires --print-rules; it checks nothing on its own\n' >&2
	exit 2
fi
if [[ -n "$DATABASE" && ! -f "$DATABASE" ]]; then
	printf 'tool-pins: %s does not exist, so there is no database to parse\n' "$DATABASE" >&2
	exit 2
fi
if [[ -z "$DATABASE" && ! -f "$MAKEFILE" ]]; then
	printf 'tool-pins: %s does not exist, so this gate would check nothing\n' "$MAKEFILE" >&2
	exit 2
fi

# `make -q` exits 1 when a target needs remaking, which is the normal case here
# and says nothing about the parse. Only rc 2 is a make error. `-n` keeps every
# recipe unrun; the database is printed either way.
make_db() {
	local rc=0
	make -f "$MAKEFILE" -pnq --no-print-directory "$@" 2>/dev/null || rc=$?
	if ((rc > 1)); then
		printf 'tool-pins: make could not read %s (rc %d), so no rule is derivable\n' "$MAKEFILE" "$rc" >&2
		exit 2
	fi
}

# The rules in the `# Files` section, one `target|normal-prereqs` line per
# `.build/` target. `# Not a target:` marks the entries make knows only as
# prerequisites, and its block must not be read as a rule with no prerequisites,
# which is the exact finding this gate reports.
build_rules() {
	awk -v root="$REPO_ROOT/" '
		/^# Files$/        { in_files = 1; next }
		!in_files          { next }
		/^# Not a target:/ { skip = 1; next }
		/^#/               { next }
		/^[^ \t]/ {
			if (skip) { skip = 0; next }
			if ($0 !~ /^[^ \t=]+:([^=]|$)/) next
			i = index($0, ":")
			target = substr($0, 1, i - 1)
			prereqs = substr($0, i + 1)
			# A target-specific variable, which GNU make 4.x prints in this
			# rule position: `target: VAR := value`. Its right-hand side is a
			# value, not a prerequisite list.
			if (prereqs ~ /^[ \t]*[A-Za-z_][A-Za-z0-9_]*[ \t]*[:+?]?=/) next
			# Order-only prerequisites do not make a target out of date, so
			# everything after the pipe is dropped before the count.
			j = index(prereqs, "|")
			if (j > 0) prereqs = substr(prereqs, 1, j - 1)
			sub(root, "", target)
			if (target !~ /(^|\/)\.build\//) next
			gsub(root, "", prereqs)
			gsub(/^[ \t]+|[ \t]+$/, "", prereqs)
			print target "|" prereqs
		}
		{ skip = 0 }
	'
}

# The `*_VERSION` variables the makefile itself sets. `# default` marks make's
# own (MAKE_VERSION), which is not a pin and is read-only.
version_vars() {
	awk '
		/^# makefile/ { from_makefile = 1; next }
		/^[A-Za-z_][A-Za-z0-9_]*_VERSION[ \t]*[:?]?=/ {
			if (from_makefile) { split($0, f, /[ \t]*[:?]?=/); print f[1] }
		}
		{ from_makefile = 0 }
	' | sort -u
}

if [[ -n "$DATABASE" ]]; then
	# The parser seam: no make run, no probe, just the rules the dump yields.
	build_rules < "$DATABASE"
	exit 0
fi

baseline="$(make_db | build_rules)"
if [[ -z "$baseline" ]]; then
	printf 'tool-pins: %s declares no .build/ rule, so this gate would check nothing\n' "$MAKEFILE" >&2
	exit 2
fi

overrides=()
while IFS= read -r var; do
	[[ -n "$var" ]] || continue
	overrides+=("$var=$PROBE_VERSION")
done < <(make_db | version_vars)
probe="$(make_db "${overrides[@]+"${overrides[@]}"}" | build_rules)"

# Every `.build/` path named as a prerequisite of a `.build/` target: the pin
# sentinels. Derived rather than listed, so a second one cannot be forgotten.
sentinels="$(awk -F'|' '{ n = split($2, ps, / /); for (i = 1; i <= n; i++) if (ps[i] ~ /\.build\//) print ps[i] }' <<<"$baseline" | sort -u)"

tracked="$(git ls-files)"

fails=0
tools=0
fail() {
	printf 'tool-pins: %s\n' "$1" >&2
	((fails++)) || true
}

while IFS='|' read -r target prereqs; do
	if grep -qxF -- "$target" <<<"$sentinels"; then
		continue
	fi
	if [[ -z "${prereqs// /}" ]]; then
		fail "\`$target\` declares no prerequisite that can make it out of date, so make serves an existing binary forever and a version bump keeps running the old tool (Q842/Q857)
       depend on the file carrying its version pin — a tracked manifest, or a version-keyed sentinel under .build/ the way COSIGN_PIN is"
		continue
	fi
	# The same target's prerequisites with the version variables overridden,
	# one per line so the comparison is a whole path rather than a substring. A
	# path that moved is keyed on a pin; one that did not must be tracked.
	probed="$(awk -F'|' -v t="$target" '$1 == t { print $2 }' <<<"$probe" | tr ' ' '\n')"
	((tools++)) || true
	for p in $prereqs; do
		if grep -qxF -- "$p" <<<"$tracked"; then
			continue
		fi
		if ! grep -qxF -- "$p" <<<"$probed"; then
			continue
		fi
		fail "\`$target\` depends on \`$p\`, which is neither tracked nor keyed on a version pin — it does not move when the pin does, so the rule never fires on a bump (Q857)
       key the sentinel's name on the version variable, the way COSIGN_PIN carries \$(COSIGN_VERSION)"
	done
done <<<"$baseline"

if ((fails > 0)); then
	printf '\n%d tool-pin check(s) failed. Every .build/ tool rule declares the file that\n' "$fails" >&2
	printf 'carries its version pin as a prerequisite; without one a bump serves the old\n' >&2
	printf 'binary against the new target.\n' >&2
	exit 1
fi

printf 'tool pins: %d .build/ tool rule(s) each declare a version-pin prerequisite\n' "$tools"
