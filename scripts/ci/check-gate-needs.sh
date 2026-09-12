#!/usr/bin/env bash
#
# Reconcile every `*-gate` aggregator job's `needs:` with its workflow's job ids
# (Q856).
#
# A path-gated workflow reports one required check: a `*-gate` job that `needs:`
# every real job and passes when they all did. The branch protection names the
# gate, never the jobs, so a job left out of that list runs and reports red while
# the required check reports green and blocks nothing. It is the same false
# negative check-path-filters.sh exists for, arriving from the other direction: a
# filter that omits a path makes a gate green by SKIPPING, and a `needs:` that
# omits a job makes it green by NOT WAITING.
#
# Q845 fixed the one live instance (`uses-pinned`, absent from unit-test.yml's
# gate). A sweep of the aggregators found no other, so this is a recurrence
# guard rather than a repair, which is exactly the kind that has to be mechanical:
# the defect is invisible on a green PR, and the list grows by hand every time
# someone adds a job.
#
# One assertion. For each workflow, every job whose id ends `-gate` must `needs:`
# every other job in that workflow that is not itself a `-gate`. Gate jobs are
# excluded from the requirement rather than forbidden: no workflow here has two,
# and an aggregator waiting on an aggregator is a shape to decide on rather than
# to mandate.
#
# Two refusals, both shapes that would otherwise pass by checking nothing: a
# workflow directory that resolved to no files, and a tree with no `-gate` job in
# it at all.
#
# Usage: check-gate-needs.sh [--dir <workflows dir>]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

WF_DIR="$REPO_ROOT/.github/workflows"
while (($# > 0)); do
	case "$1" in
	--dir)
		WF_DIR="$2"
		shift 2
		;;
	*)
		printf 'check-gate-needs: unknown argument %s\n' "$1" >&2
		exit 2
		;;
	esac
done

# A job a gate deliberately does not wait on. Empty today, and an entry must say
# why: the whole value of this gate is that the list cannot grow silently, so an
# exemption belongs in the diff where a reviewer meets it. Format is
# `<workflow.yml>:<gate job>:<job id>`.
EXEMPT=(
)

PATHFILTERS_BIN="$REPO_ROOT/.build/pathfilters"

ensure_pathfilters() {
	[[ -x "$PATHFILTERS_BIN" ]] && return 0
	require_cmd go "https://go.dev/dl/"
	mkdir -p "$REPO_ROOT/.build"
	# devtools/ is outside the Go workspace, hence GOWORK=off — see
	# docs/development/go-workspaces.md.
	(cd "$REPO_ROOT/devtools" && GOWORK=off go build -o "$PATHFILTERS_BIN" ./ci/pathfilters)
}

is_exempt() {
	local key="$1" e
	for e in ${EXEMPT[@]+"${EXEMPT[@]}"}; do
		[[ "$e" == "$key" ]] && return 0
	done
	return 1
}

main() {
	if [[ ! -d "$WF_DIR" ]]; then
		printf 'check-gate-needs: %s is not a directory, so this gate would check nothing\n' "$WF_DIR" >&2
		exit 2
	fi
	ensure_pathfilters

	local -a workflows=()
	local f
	for f in "$WF_DIR"/*.yml "$WF_DIR"/*.yaml; do
		[[ -f "$f" ]] && workflows+=("$f")
	done
	if ((${#workflows[@]} == 0)); then
		printf 'check-gate-needs: no workflows under %s, so this gate would check nothing\n' "$WF_DIR" >&2
		exit 2
	fi

	local errors=0 gates_seen=0 pairs=0
	for f in "${workflows[@]}"; do
		local base parsed
		base="$(basename "$f")"
		parsed="$("$PATHFILTERS_BIN" jobs "$f")" || {
			printf 'check-gate-needs: could not read the jobs of %s\n' "$base" >&2
			exit 2
		}
		[[ -z "$parsed" ]] && continue

		# Job ids in document order, and each job's needs as one space-padded
		# string so a substring test cannot match a prefix of another id.
		local ids needs_of
		ids="$(awk -F'\t' '!seen[$1]++ { print $1 }' <<<"$parsed")"
		local gate
		while read -r gate; do
			[[ "$gate" == *-gate ]] || continue
			gates_seen=$((gates_seen + 1))
			needs_of=" $(awk -F'\t' -v g="$gate" '$1==g && $2!="" { printf "%s ", $2 }' <<<"$parsed")"
			local job
			while read -r job; do
				[[ -z "$job" || "$job" == "$gate" || "$job" == *-gate ]] && continue
				pairs=$((pairs + 1))
				if [[ "$needs_of" == *" $job "* ]]; then
					continue
				fi
				if is_exempt "$base:$gate:$job"; then
					continue
				fi
				printf 'check-gate-needs: %s: job %s is not in the needs of %s, so it can report red while the required check reports green\n' \
					"$base" "$job" "$gate" >&2
				errors=$((errors + 1))
			done <<<"$ids"
		done <<<"$ids"
	done

	if ((gates_seen == 0)); then
		printf 'check-gate-needs: no *-gate job found under %s, so this gate would check nothing\n' "$WF_DIR" >&2
		exit 2
	fi
	if ((errors > 0)); then
		printf '\nAdd the job to that gate'"'"'s needs list. Branch protection names the gate and never the jobs, so a job the gate does not wait on blocks nothing (Q845, Q856).\n' >&2
		exit 1
	fi
	printf 'check-gate-needs: ok (%d gate job(s) waiting on %d sibling job(s))\n' "$gates_seen" "$pairs"
}

main
