#!/usr/bin/env bash
#
# Reconcile the hand-maintained `dorny/paths-filter` lists in .github/workflows/
# against what the repo actually contains (Q429).
#
# The path-gated workflows classify each PR's diff and skip their expensive jobs
# when nothing they cover changed, so a filter that omits a directory makes its
# gate go green by SKIPPING rather than by passing — the most expensive kind of
# false negative, because `main` ends up green on evidence it never gathered.
# Nothing reconciled those lists with `go.work`, and both modules added after the
# filters were first written hit it: `api/` and `scaleset/` were absent from the
# integration, e2e, and security filters, so an api- or scaleset-only change
# skipped envtest, e2e, govulncheck, and trivy entirely. Q400 fixed four
# workflows by hand; this gate is the recurrence guard.
#
# Four assertions, cheapest first:
#
#   1. Registry completeness. Every filter in every `filters:` block is listed
#      below as either workspace-covering or narrow-by-design. A new workflow, or
#      a new filter in an existing one, fails this gate until someone decides
#      which it is — so the hole cannot reopen in a new shape.
#   2. Module coverage. Every workspace-covering filter matches every module in
#      `go.work`. Adding a module to the workspace without widening those filters
#      fails here, naming the module, the workflow, and the pattern to add.
#   3. Live paths. Every pattern's literal prefix still exists on disk. A renamed
#      or deleted script leaves a pattern matching nothing, which silently
#      narrows its gate the same way a missing module does.
#   4. Shared-lane agreement. Two filters gating the same reusable workflow list
#      the same scripts/ patterns. e2e-test.yml and e2e-calico.yml both call
#      e2e-reusable.yml, yet disagreed by ~60× about which scripts it runs —
#      calico named 2 of the ≥6, so a free-runner-disk.sh change skipped the lane
#      that exercises it (Q571).
#
# Costs a fraction of a second: it parses YAML and stats paths, it compiles
# nothing. Backs `make path-filters-check` (part of `make check`) and the
# `path-filters` job in .github/workflows/unit-test.yml. Its behavioural tests
# are scripts/ci/check-path-filters-test.sh.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

WORKFLOW_DIR=".github/workflows"

# The glob metacharacters a picomatch pattern can carry, as a bracket expression
# (']' leads, where it is literal). Held in variables so the `=~` right-hand sides
# below stay unquoted — which is what makes bash treat them as regexes — without
# putting bare parentheses in the source, where they read as a subshell to any
# reader (and to shells with different quoting rules).
GLOB_METACHARS='[][*?!()]'
# The same set negated and anchored, capturing everything a pattern pins literally
# before its first metacharacter.
LITERAL_HEAD='^([^][*?!()]*)'

# Filters that must cover EVERY go.work module, as "<workflow>:<filter>". A
# filter belongs here when the jobs it gates compile, test, scan, or bake the
# whole workspace — ask what the gate actually does, not what its list happens to
# say today. Assertion 2 enforces the coverage; assertion 1 makes forgetting to
# classify a new filter a failure rather than a silent omission.
WORKSPACE_FILTERS=(
	'unit-test.yml:code'         # make lint + the -race unit suite + coverage, per module
	'integration-test.yml:code'  # the envtest suites, which import across modules
	'e2e-test.yml:e2e'           # bakes and deploys the images every module links into
	'security-scan.yml:code'     # go-vulncheck.sh loops workspace_modules; trivy scans the images
)

# Filters deliberately NARROWER than the workspace. Each is scoped to the inputs
# of one specific gate, so widening it to every module would just re-run that
# gate on changes that cannot affect it. Reasons are per-entry because the reason
# is the whole content of the decision.
NARROW_FILTERS=(
	'unit-test.yml:status'            # docs/STATUS.md and its linter
	'unit-test.yml:scripts'           # the scripts/ + hooks/ trees shellcheck and scripts-test gate
	'unit-test.yml:vendor'            # only what determines committed vendor/ contents
	'unit-test.yml:modules'           # only what determines the tidy-canonical go.mod/go.sum shape
	'unit-test.yml:workflows'         # the workflow filters this gate itself lints
	'unit-test.yml:claude_usage'      # the claude-usage/ Python module and its stdlib-only suite
	'security-scan.yml:chart'         # the Helm chart the Polaris posture scan renders
	'e2e-calico.yml:calico'           # NetworkPolicy/proxy code only; other PRs stay on the kindnet leg
	'manifest-validate.yml:manifests' # generated YAML, not the Go types behind it
	'license-notices.yml:notices'     # vendor/ and the notices generator
	'status-lint.yml:status'          # docs/STATUS.md, docs/roadmap.md, and their linters
	'plan-hygiene.yml:plan'           # the plan index plus any .go file (for plan-ref scanning)
	'autoscaler-drift.yml:autoscaler' # the CA/kwok pins, the kwok manifests, and the matcher under test
	'autoscaler-drift.yml:karpenter'  # the Karpenter pins/recipe and its live test (Q479)
)

# Filter pairs that gate the SAME reusable workflow and must therefore agree on
# which scripts/ groups feed it, as "<workflow>:<filter>|<workflow>:<filter>".
# Only the scripts/ patterns are compared — the rest of each filter is what makes
# the two lanes different (the calico lane is scoped to NetworkPolicy/proxy code).
SHARED_LANE_FILTERS=(
	'e2e-test.yml:e2e|e2e-calico.yml:calico' # both call .github/workflows/e2e-reusable.yml
)

# parse_filters WORKFLOW_PATH — print one "<filter>\t<pattern>" row per pattern in
# the file's `filters: |` block. The block is a YAML string nested inside YAML, so
# it is read positionally: everything indented deeper than the `filters:` key,
# until a line dedents back to it. Inside, a `name:` line opens a filter and a
# `- pattern` line adds to it. Quote handling is index/substr rather than regex so
# it works on both gawk and the BSD awk macOS ships.
parse_filters() {
	awk '
	function value(s,   quote, first, closing) {
		sub(/^-[[:space:]]*/, "", s)
		quote = sprintf("%c", 39)
		first = substr(s, 1, 1)
		if (first == quote || first == "\"") {
			s = substr(s, 2)
			closing = index(s, first)
			if (closing > 0) s = substr(s, 1, closing - 1)
			return s
		}
		# Unquoted: a trailing comment and surrounding space are not part of the pattern.
		sub(/[[:space:]]*#.*$/, "", s)
		sub(/[[:space:]]+$/, "", s)
		return s
	}
	!inblock {
		if ($0 ~ /^[[:space:]]*filters:[[:space:]]*\|[[:space:]]*$/) {
			match($0, /^[[:space:]]*/)
			base = RLENGTH
			inblock = 1
			filter = ""
		}
		next
	}
	{
		if ($0 ~ /^[[:space:]]*$/) next
		match($0, /^[[:space:]]*/)
		indent = RLENGTH
		if (indent <= base) { inblock = 0; next }
		line = substr($0, indent + 1)
		if (line ~ /^#/) next
		if (line ~ /^-([[:space:]]|$)/) {
			if (filter != "") print filter "\t" value(line)
			next
		}
		if (line ~ /:[[:space:]]*$/) {
			sub(/:[[:space:]]*$/, "", line)
			filter = line
		}
	}
	' "$1"
}

# pattern_covers_dir PATTERN DIR — true when PATTERN matches every file under DIR.
# Only a recursive glob rooted at DIR or one of its ancestors does that: a bare
# `api` matches the literal path `api` and nothing beneath it (picomatch, which
# paths-filter uses, does not expand a directory to its tree), and `api/config/**`
# leaves the rest of the module ungated. A glob metacharacter inside the prefix
# makes the covered set unknowable by textual comparison, so it counts as no
# coverage — under-counting produces a fixable failure, over-counting reopens
# exactly the hole this gate closes.
pattern_covers_dir() {
	local pattern="$1" dir="$2" prefix
	[[ "$pattern" == '**' ]] && return 0
	[[ "$pattern" == *'/**' ]] || return 1
	prefix="${pattern%/**}"
	[[ "$prefix" =~ $GLOB_METACHARS ]] && return 1
	[[ "$dir" == "$prefix" || "$dir" == "$prefix"/* ]]
}

# literal_prefix PATTERN — print the longest leading path that PATTERN pins
# literally, or nothing when it pins none. A wholly literal pattern is its own
# prefix; otherwise only the whole segments before the first metacharacter count,
# so `scripts/!(dogfood)/**` yields `scripts` and `**/go.mod` yields nothing.
literal_prefix() {
	local pattern="$1" cut="$1"
	if [[ "$pattern" =~ $LITERAL_HEAD ]]; then
		cut="${BASH_REMATCH[1]}"
	fi
	if [[ "$cut" == "$pattern" ]]; then
		printf '%s' "$pattern"
		return 0
	fi
	[[ "$cut" == */* ]] || return 0
	printf '%s' "${cut%/*}"
}

# contains ITEM ELEMENT... — true when ITEM equals one of the remaining args.
contains() {
	local item="$1" element
	shift
	for element in "$@"; do
		[[ "$element" == "$item" ]] && return 0
	done
	return 1
}

failures=0

fail() {
	echo "ERROR: $*" >&2
	failures=$((failures + 1))
}

# assert_registry_complete FOUND... — every filter present in the tree is
# classified, and every classified filter is still present.
assert_registry_complete() {
	local found=("$@") registered=("${WORKSPACE_FILTERS[@]}" "${NARROW_FILTERS[@]}") key
	for key in "${found[@]}"; do
		contains "$key" "${registered[@]}" && continue
		fail "$WORKFLOW_DIR/${key%%:*} declares filter '${key#*:}', which this gate does not know about.
  Decide what the jobs it gates actually exercise, then add '$key' to either
  WORKSPACE_FILTERS (it must cover every go.work module) or NARROW_FILTERS (it is
  scoped to one gate's inputs — say why) in scripts/ci/check-path-filters.sh."
	done
	for key in "${registered[@]}"; do
		contains "$key" "${found[@]}" && continue
		fail "scripts/ci/check-path-filters.sh registers '$key', but $WORKFLOW_DIR/${key%%:*} declares no such filter.
  Drop the stale entry, or fix the name if the filter was renamed."
	done
}

# assert_module_coverage MODULE... — every workspace-covering filter matches every
# module. Reported per (filter, module) so one run lists everything to add.
assert_module_coverage() {
	local modules=("$@") key workflow filter module pattern covered
	for key in "${WORKSPACE_FILTERS[@]}"; do
		workflow="${key%%:*}"
		filter="${key#*:}"
		for module in "${modules[@]}"; do
			covered=0
			while IFS=$'\t' read -r name pattern; do
				[[ "$name" == "$filter" ]] || continue
				if pattern_covers_dir "$pattern" "$module"; then
					covered=1
					break
				fi
			done < <(parse_filters "$WORKFLOW_DIR/$workflow")
			((covered)) && continue
			fail "$WORKFLOW_DIR/$workflow filter '$filter' does not cover go.work module '$module'.
  That gate compiles, scans, or bakes the whole workspace, so a change confined to
  '$module' would skip it and the gate would report green without testing anything
  (Q400). Add \"- '$module/**'\" to the '$filter' filter."
		done
	done
}

# assert_paths_live FOUND... — every pattern still pins a path that exists. A
# pattern left behind by a rename matches nothing and narrows its gate silently.
assert_paths_live() {
	local key workflow filter pattern prefix
	for key in "$@"; do
		workflow="${key%%:*}"
		filter="${key#*:}"
		while IFS=$'\t' read -r name pattern; do
			[[ "$name" == "$filter" ]] || continue
			prefix="$(literal_prefix "$pattern")"
			[[ -n "$prefix" ]] || continue
			[[ -e "$prefix" ]] && continue
			fail "$WORKFLOW_DIR/$workflow filter '$filter' lists '$pattern', but '$prefix' does not exist.
  The pattern matches nothing, so whatever it used to gate is now ungated. Point it
  at the current path, or remove it if the gate no longer needs it."
		done < <(parse_filters "$WORKFLOW_DIR/$workflow")
	done
}

# scripts_patterns WORKFLOW FILTER — print FILTER's scripts/ patterns, sorted, one
# per line. Sorted so the comparison is order-insensitive: the two lanes may list
# the groups in whatever order reads best next to their own comments.
scripts_patterns() {
	local workflow="$1" filter="$2" name pattern
	while IFS=$'\t' read -r name pattern; do
		[[ "$name" == "$filter" ]] || continue
		[[ "$pattern" == scripts/* ]] || continue
		printf '%s\n' "$pattern"
	done < <(parse_filters "$WORKFLOW_DIR/$workflow") | LC_ALL=C sort
}

# assert_shared_lanes_agree — paired filters gating one reusable workflow name the
# same scripts/ groups. A diff either way is a bug: the shorter list skips a lane
# on a change that lane runs, the longer one runs a lane on a change it does not.
assert_shared_lanes_agree() {
	local pair left right left_patterns right_patterns
	for pair in "${SHARED_LANE_FILTERS[@]}"; do
		left="${pair%%|*}"
		right="${pair#*|}"
		left_patterns="$(scripts_patterns "${left%%:*}" "${left#*:}")"
		right_patterns="$(scripts_patterns "${right%%:*}" "${right#*:}")"
		[[ "$left_patterns" == "$right_patterns" ]] && continue
		fail "$WORKFLOW_DIR/${left%%:*} filter '${left#*:}' and $WORKFLOW_DIR/${right%%:*} filter
  '${right#*:}' gate the same reusable workflow but list different scripts/ patterns:
$(diff <(printf '%s\n' "$left_patterns") <(printf '%s\n' "$right_patterns") | sed 's/^/    /')
  '<' is ${left%%:*} only, '>' is ${right%%:*} only. Whichever lane is missing a
  pattern skips on a change it actually runs (Q571). Make the two sets identical."
	done
}

main() {
	local modules=() found=() workflow filter module_dir
	while IFS= read -r module_dir; do
		# go.work reports './api'; the filters are repo-relative without the './'.
		modules+=("${module_dir#./}")
	done < <(workspace_modules)

	for workflow in "$WORKFLOW_DIR"/*.yml; do
		while IFS=$'\t' read -r filter _; do
			contains "$(basename "$workflow"):$filter" "${found[@]:-}" && continue
			found+=("$(basename "$workflow"):$filter")
		done < <(parse_filters "$workflow")
	done

	# Zero filters means the parser stopped matching, not that the workflows are
	# clean — fail loudly rather than reporting a vacuous pass.
	((${#found[@]} > 0)) || die "no 'filters:' block found under $WORKFLOW_DIR. Either the workflows no longer
  use dorny/paths-filter, or parse_filters in $0 has stopped matching them."

	echo "==> checking ${#found[@]} path filter(s) against ${#modules[@]} go.work module(s)"

	assert_registry_complete "${found[@]}"
	assert_module_coverage "${modules[@]}"
	assert_paths_live "${found[@]}"
	assert_shared_lanes_agree

	if ((failures > 0)); then
		echo >&2
		echo "path-filter check failed: $failures problem(s). See docs/development/testing.md" >&2
		echo "§ Path-gated workflows for why a skipped gate is worse than a failing one." >&2
		exit 1
	fi
	echo "path filters cover every go.work module and pin only live paths"
}

# Run main only when executed directly, so check-path-filters-test.sh can source
# this file to exercise the helpers against fixtures.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
