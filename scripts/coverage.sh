#!/usr/bin/env bash
#
# Measure per-module unit-test coverage and gate it against a recorded baseline.
#
# The repo is a Go workspace, so coverage — like the unit tests themselves — is
# measured per module (`go test -coverprofile` in each `go.work` Use directory),
# never with a repo-root `go test ./...`. Each module's number is the aggregate
# statement coverage reported by `go tool cover -func`, computed over a profile
# from which generated and thin wiring code has been filtered out (see
# EXCLUDE_RE) so the floor reflects hand-written logic, not boilerplate that
# churns whenever a CRD field is added or a binary is rewired.
#
# We gate by a *no-regression ratchet*, not an absolute percentage target: the
# baseline in coverage-baseline.txt records each module's floor, and `check`
# fails only if a module drops more than TOLERANCE below its floor. This avoids
# manufacturing low-value tests to hit an arbitrary bar while still catching a
# real coverage regression between sessions. See docs/development/testing.md
# (§"Coverage measurement and the ratchet") and docs/plan/release-1.0.md §F.
#
# Modes:
#   report   Run coverage and print the per-module table. Writes nothing.
#   update   Run coverage and (re)write coverage-baseline.txt. Use to record a
#            new floor after intentionally adding tests (coverage went UP) — or
#            to rebase the floor down with an explicit, reviewable diff.
#   check    Run coverage and fail if any module is below its baseline floor
#            minus TOLERANCE. This is the gate CI and `make cover-check` run.
#
# A bare `go test` here is rewritten to carry the local-throttle prefix by the
# Claude Code go-throttle hook, and the loop also applies scripts/local-throttle.sh
# itself, so a manual run on a GUI dev machine stays desktop-safe; on CI/headless
# the prefix is empty and it runs at full speed (same convention as `make test`).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

BASELINE_FILE="${BASELINE_FILE:-coverage-baseline.txt}"

# Tolerance, in percentage points, that a module may drop below its recorded
# floor without failing the gate. Coverage is deterministic (the unit gate runs
# without -race), so this is not for flake: it absorbs benign denominator drift
# — e.g. adding a couple of uncovered boilerplate lines to an otherwise-covered
# package marginally dilutes the ratio without removing any test. 0.5pp is small
# enough to still catch a real regression (deleting a tested function, gutting a
# test) on any module of meaningful size.
TOLERANCE="${COVERAGE_TOLERANCE:-0.5}"

# Files excluded from the coverage profile before the percentage is computed, so
# the floor reflects hand-written logic rather than mechanically-generated code
# that would churn the number without any test change:
#   zz_generated.*       controller-gen DeepCopy methods
#   groupversion_info.go kubebuilder scheme-registration boilerplate
#
# We deliberately do NOT exclude main.go. In this repo several binaries
# (cmd/worker, cmd/proxy) keep real, unit-tested logic in their `package main`,
# so a blanket main.go exclusion would hide tested logic and leave those modules
# ungated — the opposite of the intent. The genuinely-thin entrypoints
# (cmd/agc, cmd/gmc) instead contribute a lower but still-defended floor, which
# costs the ratchet nothing: a lower floor never causes a false failure, and the
# only thing that grows mechanically without a test change (generated code) is
# already filtered above.
EXCLUDE_RE='(zz_generated.*\.go|groupversion_info\.go)'

MODULES=$(go work edit -json | jq -r '.Use[].DiskPath')

THROTTLE_PREFIX="$("$REPO_ROOT/scripts/local-throttle.sh" prefix)"

# measure_module DIR -> echoes "DIR<TAB>PCT" (PCT is "n/a" when the module has
# no statements covered by any test, e.g. a module with no _test.go files).
measure_module() {
	local dir="$1"
	local profile filtered total pct
	profile="$(mktemp "${TMPDIR:-/tmp}/cover.XXXXXX.out")"
	filtered="$(mktemp "${TMPDIR:-/tmp}/cover.XXXXXX.filtered")"
	# shellcheck disable=SC2064
	trap "rm -f '$profile' '$filtered'" RETURN

	# Run the module's unit tests with coverage. A module with no tests produces
	# "[no test files]" lines and an empty/headers-only profile — handled below.
	( cd "$dir" && $THROTTLE_PREFIX go test -timeout 2m -coverprofile="$profile" ./... >/dev/null 2>&1 ) || {
		echo "coverage: 'go test' failed in $dir" >&2
		( cd "$dir" && $THROTTLE_PREFIX go test -timeout 2m ./... ) >&2 || true
		exit 1
	}

	if [[ ! -s "$profile" ]]; then
		printf '%s\t%s\n' "$dir" "n/a"
		return
	fi

	# Keep the `mode:` header and drop any profiled line whose file path matches
	# an excluded pattern, then let `go tool cover -func` recompute the total
	# over what remains.
	{ head -n1 "$profile"; grep -vE "$EXCLUDE_RE" "$profile" | tail -n +2 || true; } >"$filtered"

	if [[ "$(wc -l <"$filtered")" -le 1 ]]; then
		# Header only — every covered statement was in excluded files.
		printf '%s\t%s\n' "$dir" "n/a"
		return
	fi

	total="$(go tool cover -func="$filtered" | tail -n1)"
	pct="$(awk '{print $NF}' <<<"$total" | tr -d '%')"
	printf '%s\t%s\n' "$dir" "$pct"
}

measure_all() {
	local dir
	for dir in $MODULES; do
		measure_module "$dir"
	done
}

cmd_report() {
	echo "Per-module unit-test coverage (generated/wiring code excluded):"
	measure_all | while IFS=$'\t' read -r dir pct; do
		printf '  %-20s %s\n' "$dir" "${pct}$([[ "$pct" != "n/a" ]] && echo '%')"
	done
}

cmd_update() {
	local tmp
	tmp="$(mktemp "${TMPDIR:-/tmp}/cover-baseline.XXXXXX")"
	{
		echo "# Per-module unit-test coverage baseline (no-regression ratchet floor)."
		echo "# Regenerate with: make cover-update   (or scripts/coverage.sh update)"
		echo "# Format: <module-disk-path><TAB><percent>   (n/a = no measurable coverage)"
		echo "# The gate (make cover-check) fails if a module drops > ${TOLERANCE}pp below its floor."
		measure_all
	} >"$tmp"
	mv "$tmp" "$BASELINE_FILE"
	echo "wrote $BASELINE_FILE"
	grep -v '^#' "$BASELINE_FILE" | while IFS=$'\t' read -r dir pct; do
		printf '  %-20s %s\n' "$dir" "${pct}$([[ "$pct" != "n/a" ]] && echo '%')"
	done
}

cmd_check() {
	if [[ ! -f "$BASELINE_FILE" ]]; then
		echo "coverage: no baseline at $BASELINE_FILE — run 'make cover-update' first" >&2
		exit 1
	fi

	local current failed=0
	current="$(measure_all)"

	# Compare each baseline floor against the current measurement.
	local dir floor now
	while IFS=$'\t' read -r dir floor; do
		[[ "$dir" =~ ^#.*$ || -z "$dir" ]] && continue
		now="$(awk -F'\t' -v d="$dir" '$1==d{print $2}' <<<"$current")"
		if [[ -z "$now" ]]; then
			echo "coverage: FAIL $dir — in baseline but not measured (module removed from go.work?)" >&2
			failed=1
			continue
		fi
		# A floor of "n/a" or a numerically-zero floor defends nothing: you
		# can't regress below "no coverage". Treating 0 like n/a also makes the
		# gate robust to a no-test module reporting an empty profile (n/a) vs a
		# 0.0% profile across Go versions — both mean the same here.
		if [[ "$floor" == "n/a" ]] || awk -v f="$floor" 'BEGIN{exit !(f+0==0)}'; then
			printf '  %-20s %s (no floor)\n' "$dir" "${now}$([[ "$now" != "n/a" ]] && echo '%')"
			continue
		fi
		if [[ "$now" == "n/a" ]]; then
			echo "coverage: FAIL $dir — had a ${floor}% floor but now measures no coverage" >&2
			failed=1
			continue
		fi
		# now >= floor - TOLERANCE ?
		if awk -v n="$now" -v f="$floor" -v t="$TOLERANCE" 'BEGIN{exit !(n + t < f)}'; then
			printf '  %-20s %s%%  FAIL (floor %s%%, tolerance %spp)\n' "$dir" "$now" "$floor" "$TOLERANCE" >&2
			failed=1
		else
			printf '  %-20s %s%%  ok (floor %s%%)\n' "$dir" "$now" "$floor"
		fi
	done <"$BASELINE_FILE"

	# Warn (do not fail) when a module's coverage has risen well above its floor:
	# a good moment to ratchet the baseline up with `make cover-update`.
	while IFS=$'\t' read -r dir now; do
		floor="$(awk -F'\t' -v d="$dir" '!/^#/ && $1==d{print $2}' "$BASELINE_FILE")"
		[[ -z "$floor" || "$floor" == "n/a" || "$now" == "n/a" ]] && continue
		if awk -v n="$now" -v f="$floor" 'BEGIN{exit !(n > f + 2)}'; then
			# shellcheck disable=SC2016  # backticks are literal text in the message
			printf '  note: %s rose to %s%% (floor %s%%) — consider `make cover-update`\n' "$dir" "$now" "$floor"
		fi
	done <<<"$current"

	if [[ "$failed" -ne 0 ]]; then
		echo "coverage: regression below baseline floor — add tests or, if intentional, rebaseline with 'make cover-update'" >&2
		exit 1
	fi
	echo "coverage: all modules at or above their baseline floor"
}

case "${1:-report}" in
	report) cmd_report ;;
	update) cmd_update ;;
	check)  cmd_check ;;
	*) echo "usage: $0 {report|update|check}" >&2; exit 2 ;;
esac
