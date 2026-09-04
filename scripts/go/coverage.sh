#!/usr/bin/env bash
#
# Measure per-module unit-test coverage and gate it against a recorded baseline.
#
# The repo is a Go workspace, so coverage — like the unit tests themselves — is
# reported per module, never with a repo-root `go test ./...`. It is *measured*
# with one workspace-wide `go test -coverprofile` over an explicit `./<module>/...`
# pattern per go.work module (the same shape scripts/go/go-test.sh uses for `make
# test`), and the merged profile is then split back per module. One invocation
# lets Go schedule the whole workspace as a single build graph, so the many
# small modules overlap with the big cmd/agc / cmd/gmc dependency compiles
# instead of queueing behind them. Each module's number is the aggregate
# statement coverage reported by `go tool cover -func`, computed over its slice
# of the profile with generated and thin wiring code filtered out (see
# EXCLUDE_RE) so the floor reflects hand-written logic, not boilerplate that
# churns whenever a CRD field is added or a binary is rewired.
#
# We gate by a *no-regression ratchet*, not an absolute percentage target: the
# baseline in coverage-baseline.txt records each module's floor, and `check`
# fails only if a module drops more than TOLERANCE below its floor. This avoids
# manufacturing low-value tests to hit an arbitrary bar while still catching a
# real coverage regression between sessions. See docs/development/testing.md
# (§"Coverage measurement and the ratchet").
#
# Modes:
#   report   Run coverage and print the per-module table. Writes nothing.
#   update   Run coverage and (re)write coverage-baseline.txt. Use to record a
#            new floor after intentionally adding tests (coverage went UP) — or
#            to rebase the floor down with an explicit, reviewable diff.
#   check    Run coverage and fail if any module is below its baseline floor
#            minus TOLERANCE. This is the gate CI and `make cover-check` run.
#
# The -timeout below is 5m, not Go's 10m default and not the 2m this used to
# carry. It is a hang canary, and on this machine it was measuring scheduling
# instead: `go test -timeout` is per-test-binary WALL clock, so a binary that is
# not scheduled spends the budget doing nothing. Measured 2026-08-24, api/apinames
# reported 37.983s and 0.645s across two runs at an identical 98.6% coverage, and
# that package has no sleep, poll or TestMain to block on -- 58.9x on provably
# identical work. 5m is ~3.6x the worst wall observed (84.084s, agc/internal/
# controller) and still trips early enough under the coverage job's own
# timeout-minutes: 15 for Go to print its goroutine dump. Measured on that job:
# 186s total, 6s of it overhead, test binaries starting between 13s and 176s in.
# The dump prints one budget after the wedged binary starts, so 5m lands around
# 320-480s of 900 and 10m around 620-780s. Both print, so the job deadline does
# not decide between them: it caps the budget from above and the false red 2m
# produced caps it from below. What picks 5m over 10m is margin -- it clears
# that floor at the 3.6x above and holds at least twice 10m's, and a later
# start only widens that: 10m prints nothing once a binary starts 294s in.
# Rationale and the numbers: docs/development/testing.md (§"The coverage budget
# is wall clock, so it measures scheduling").
#
# A bare `go test` here is rewritten to carry the local-throttle prefix by the
# Claude Code go-throttle hook, and the run also applies scripts/agent/local-throttle.sh
# itself, so a manual run on a GUI dev machine stays desktop-safe; on CI/headless
# the prefix is empty and it runs at full speed (same convention as `make test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

BASELINE_FILE="${BASELINE_FILE:-coverage-baseline.txt}"

# How far a module may drop below its recorded floor without failing the gate:
# the LARGER of TOLERANCE percentage points and TOLERANCE_STMT statements, sized
# per module by effective_tolerance below.
#
# Two units because a statement is worth 100/N pp, so a flat pp figure buys the
# 5,523-statement cmd/agc 27.6 statements of slack and the 192-statement api
# 0.96. Both knobs are needed: pp alone fails a small module on one flipped
# block (Q803), statements alone would tighten cmd/agc to 0.05pp. Rationale and
# the measurement: docs/development/testing.md (§"Coverage measurement and the
# ratchet"). TOLERANCE_STMT sits above the observed noise (Q803's two
# statements; Q377's 1-3 load-dependent blocks) and under any real regression.
TOLERANCE="${COVERAGE_TOLERANCE:-0.5}"
TOLERANCE_STMT="${COVERAGE_TOLERANCE_STMT:-3}"

# Files excluded from the coverage profile before the percentage is computed, so
# the floor reflects production logic rather than code that churns the number
# without any test change:
#   zz_generated.*       controller-gen DeepCopy methods
#   groupversion_info.go kubebuilder scheme-registration boilerplate
#   /<pkg>test/, /<pkg>stub/, /test/   test-only helper packages (see below)
#
# Test-helper packages exist only to support other packages' tests, not to ship
# in any binary: the `<pkg>test` external-helper convention (e.g. broker/brokertest,
# an HTTP stub for the broker protocol), the `<pkg>stub` protocol-model convention
# the doubles are built on (broker/brokerstub, scaleset/scalesetstub), and anything
# under a `test/` helper tree (e.g. gmc/test/utils, the e2e kubectl/diagnostics
# helpers; test/fakegithub).
# Their own coverage is partial and irrelevant to shipped code, so folding it
# into a module's floor made the ratchet track helper code — e.g. broker measured
# ~48% blended while its production package was ~80% (Q110, sibling of Q77).
# Excluding them makes each floor track the production packages it's meant to
# defend.
#
# The `<pkg>stub` half was added with Q528, which moved 1,400 lines of scale-set
# protocol model out of scalesettest into scalesetstub so a `package main` could
# link it. Nothing about how well that code is tested changed — it is driven by
# the AGC listener suite, cmd/probe, and the fakegithub tests — but those live in
# other modules, and per-package coverage credits none of them. Counting it sank
# the scaleset module from 84.6% to 59.4% on a refactor that added tests.
#
# We deliberately do NOT exclude main.go. In this repo several binaries
# (cmd/worker, cmd/proxy) keep real, unit-tested logic in their `package main`,
# so a blanket main.go exclusion would hide tested logic and leave those modules
# ungated — the opposite of the intent. The genuinely-thin entrypoints
# (cmd/agc, cmd/gmc) instead contribute a lower but still-defended floor, which
# costs the ratchet nothing: a lower floor never causes a false failure, and the
# only thing that grows mechanically without a test change (generated code) is
# already filtered above.
EXCLUDE_RE='(zz_generated.*\.go|groupversion_info\.go|/[a-z]+test/|/[a-z]+stub/|/test/)'

# effective_tolerance NSTMT — echo how many percentage points a module of NSTMT
# statements may drop below its floor: the larger of TOLERANCE and what
# TOLERANCE_STMT statements are worth on that denominator. NSTMT of 0 or "n/a"
# yields TOLERANCE, which defends nothing anyway on a module with no statements.
effective_tolerance() {
	awk -v t="$TOLERANCE" -v s="$TOLERANCE_STMT" -v n="$1" 'BEGIN {
		per_stmt = (n + 0 > 0) ? s * 100 / (n + 0) : 0
		printf "%.4f\n", (per_stmt > t + 0) ? per_stmt : t + 0
	}'
}

# module_import_path DIR — echo the module path declared by DIR/go.mod. The
# coverage profile identifies packages by import path, so this is what maps a
# profiled line back to the go.work disk path the baseline is keyed by.
module_import_path() {
	awk '$1 == "module" { print $2; exit }' "$1/go.mod"
}

# module_profile MODPATH PROFILE — write the slice of merged profile PROFILE
# owned by module import path MODPATH: the `mode:` header, then every profiled
# line whose package path is under MODPATH/, minus files matching EXCLUDE_RE.
#
# The trailing `/` is the boundary that keeps one module path from swallowing
# another that merely shares its prefix (`.../agc` must not claim `.../agcutil`).
# Output is a valid profile `go tool cover -func` can total on its own; a
# header-only result means the module has no measurable coverage.
module_profile() {
	local modpath="$1" profile="$2"
	head -n1 "$profile"
	grep -E "^${modpath}/" "$profile" | grep -vE "$EXCLUDE_RE" || true
}

# profile_statements FILTERED -- echo the statement denominator for an
# already-filtered profile: the NumStmt column summed once per DISTINCT block,
# never once per line.
#
# A workspace-wide `go test -coverprofile` emits one row per (block, reporting
# test binary), so a package linked by three instrumented binaries is written
# three times while one served from the test cache is written once. `go tool
# cover -func` merges those rows, so the percentage is invariant to that
# multiplicity and a raw line sum is not: it tracks the cache instead of the
# tree, and effective_tolerance is sized off it. Measured 2026-09-04 over one
# unchanged tree: a colder run gave cmd/proxy 456 rows over 228 blocks (772
# statements by line sum, 0.5000pp of tolerance) and a fully cached run 228 rows
# (386, 0.7772pp), both reporting 80.8%. Q989.
#
# Only the denominator folds. Duplicate rows always agree on NumStmt but not
# always on the hit count (2 blocks of 11,031 in that run), so dropping the rows
# themselves would drop a hit and move the percentage.
profile_statements() {
	awk 'NR > 1 && !seen[$1]++ { n += $(NF - 1) } END { print n + 0 }' "$1"
}

# run_coverage PROFILE — run the unit tests across every go.work module in one
# invocation, writing the merged coverage profile to PROFILE.
run_coverage() {
	local profile="$1" dir
	local patterns=()
	for dir in $(workspace_modules); do
		patterns+=("$dir/...")
	done

	init_throttle # sets THROTTLE_JOBS + THROTTLE_PREFIX
	local p_flag=""
	[[ -n "$THROTTLE_JOBS" ]] && p_flag="-p $THROTTLE_JOBS"
	# V / VERBOSE streams `go test -v` (matches make test) for debugging a hang.
	local verbose_flag=""
	[[ -n "${V:-}${VERBOSE:-}" ]] && verbose_flag="-v"
	[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"

	# go test's per-package output goes to stderr so a green run still shows the
	# `ok pkg` lines (make test parity) while this script's STDOUT stays the
	# DIR<TAB>PCT table its callers parse. GOMAXPROCS + -p match make test's
	# throttle. -trimpath makes the test-result cache key path-independent so a
	# fresh worktree inherits an already-measured package instead of re-running
	# it (see scripts/go/go-test.sh's header).
	# A cached package replays the same blocks with the same counts, so the
	# ratchet reads the same number either way, but not the same ROWS: an
	# uncached package is written once per instrumented test binary linking it,
	# so the profile's line count tracks the cache rather than the tree. Nothing
	# downstream may sum those lines; see profile_statements (Q989).
	echo "==> go test -coverprofile ${patterns[*]}" >&2
	# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
	$THROTTLE_PREFIX go test -trimpath -timeout 5m $p_flag $verbose_flag \
		-coverprofile="$profile" "${patterns[@]}" >&2 || {
		echo "coverage: 'go test' failed (output above)" >&2
		exit 1
	}

	# Modules outside go.work cannot join the invocation above — they are a
	# separate build list, hence GOWORK=off and a run of their own — but they are
	# measured and ratcheted like any other. Each keeps its own profile rather
	# than merging into the one above, because totalling a profile compiles the
	# packages it names: `go tool cover -func` over a devtools line from the repo
	# root fails to resolve the import at all (the root is a workspace with a
	# vendor tree), so the total has to be taken from inside the module, with
	# GOWORK=off, which is what measure_all does.
	for dir in $(firstparty_nonworkspace_modules); do
		local nwprofile="$RUN_TMP/nonworkspace-${dir//\//_}.out"
		echo "==> go test -coverprofile ./$dir/... (GOWORK=off)" >&2
		(
			cd "$dir"
			export GOWORK=off
			# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
			$THROTTLE_PREFIX go test -trimpath -timeout 5m $p_flag $verbose_flag \
				-coverprofile="$nwprofile" ./... >&2
		) || {
			echo "coverage: 'go test' failed in $dir (output above)" >&2
			exit 1
		}
	done
	return 0
}

# measure_all — echo "DIR<TAB>PCT<TAB>NSTMT" per measured module: the go.work
# modules in go.work order, then the first-party modules outside it. PCT is "n/a" when the module has no statements covered by any test (e.g.
# a module with no _test.go files, or one whose every profiled file is excluded),
# and NSTMT is then 0. NSTMT is the module's statement denominator, which is what
# sizes the gate's tolerance (see effective_tolerance); the baseline file records
# only the first two fields.
measure_all() {
	local profile="$RUN_TMP/merged.out"
	run_coverage "$profile"

	local dir modpath filtered
	for dir in $(workspace_modules); do
		if [[ ! -s "$profile" ]]; then
			printf '%s\t%s\t%s\n' "$dir" "n/a" 0
			continue
		fi
		modpath="$(module_import_path "$dir")"
		filtered="$RUN_TMP/$(tr '/.' '__' <<<"$dir").filtered"
		module_profile "$modpath" "$profile" >"$filtered"
		report_module "$dir" "$filtered" ""
	done

	# Same reporting, one directory in, because the total compiles the packages.
	for dir in $(firstparty_nonworkspace_modules); do
		modpath="$(module_import_path "$dir")"
		filtered="$RUN_TMP/$(tr '/.' '__' <<<"$dir").filtered"
		module_profile "$modpath" "$RUN_TMP/nonworkspace-${dir//\//_}.out" >"$filtered"
		# Reported as ./DIR to match workspace_modules, which is the form the
		# baseline is keyed by; the bare DIR is the directory to total from.
		report_module "./$dir" "$filtered" "$dir"
	done
}

# report_module DIR FILTERED [TOTAL_IN] — echo DIR<TAB>PCT<TAB>NSTMT for one
# already-filtered profile. TOTAL_IN is the directory to total from, empty for a
# workspace module; a non-workspace module is totalled inside itself with
# GOWORK=off, since `go tool cover -func` resolves the imports it reads.
report_module() {
	local dir="$1" filtered="$2" total_in="$3" pct nstmt
	if [[ ! -s "$filtered" ]] || [[ "$(wc -l <"$filtered")" -le 1 ]]; then
		# Header only, or no profile at all — the module has no profiled lines, or
		# every covered statement was in an excluded file.
		printf '%s\t%s\t%s\n' "$dir" "n/a" 0
		return 0
	fi
	if [[ -n "$total_in" ]]; then
		pct="$(cd "$total_in" && GOWORK=off go tool cover -func="$REPO_ROOT/${filtered#"$REPO_ROOT/"}" |
			tail -n1 | awk '{print $NF}' | tr -d '%')"
	else
		pct="$(go tool cover -func="$filtered" | tail -n1 | awk '{print $NF}' | tr -d '%')"
	fi
	nstmt="$(profile_statements "$filtered")"
	printf '%s\t%s\t%s\n' "$dir" "$pct" "$nstmt"
}

cmd_report() {
	echo "Per-module unit-test coverage (generated/wiring code excluded):"
	measure_all | while IFS=$'\t' read -r dir pct nstmt; do
		printf '  %-20s %-7s (%s statements)\n' \
			"$dir" "${pct}$([[ "$pct" != "n/a" ]] && echo '%')" "$nstmt"
	done
}

cmd_update() {
	local tmp
	tmp="$(mktemp "$RUN_TMP/cover-baseline.XXXXXX")"
	{
		echo "# Per-module unit-test coverage baseline (no-regression ratchet floor)."
		echo "# Regenerate with: make cover-update   (or scripts/go/coverage.sh update)"
		echo "# Format: <module-disk-path><TAB><percent>   (n/a = no measurable coverage)"
		echo "# The gate (make cover-check) fails if a module drops more than the"
		echo "# larger of ${TOLERANCE}pp and ${TOLERANCE_STMT} statements below its floor."
		# Drop measure_all's statement-count column: the gate re-measures it, and
		# recording it here would make every refactor of a module's shape a
		# baseline diff even when its coverage did not move.
		measure_all | cut -f1,2
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
	local dir floor now nstmt tol
	while IFS=$'\t' read -r dir floor; do
		[[ "$dir" =~ ^#.*$ || -z "$dir" ]] && continue
		now="$(awk -F'\t' -v d="$dir" '$1==d{print $2}' <<<"$current")"
		nstmt="$(awk -F'\t' -v d="$dir" '$1==d{print $3}' <<<"$current")"
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
		# now >= floor - tol ? tol is sized against this module's own denominator,
		# and is printed either way so the output says what was actually allowed.
		tol="$(effective_tolerance "$nstmt")"
		if awk -v n="$now" -v f="$floor" -v t="$tol" 'BEGIN{exit !(n + t < f)}'; then
			printf '  %-20s %s%%  FAIL (floor %s%%, tolerance %spp over %s statements)\n' \
				"$dir" "$now" "$floor" "$tol" "$nstmt" >&2
			failed=1
		else
			printf '  %-20s %s%%  ok (floor %s%%, tolerance %spp)\n' "$dir" "$now" "$floor" "$tol"
			# Below the floor but inside tolerance. This is the state Q803 sat in
			# for 13 days: #1013 removed 8 statements from cmd/proxy, its number
			# moved 79.5 -> 79.3, and the gate printed a bare "ok" while the
			# shortfall ate the budget that transient noise needed. The tolerance
			# is for noise, so a floor the tree can no longer reach is a stale
			# record, not a pass.
			if awk -v n="$now" -v f="$floor" 'BEGIN{exit !(n + 0 < f + 0)}'; then
				# shellcheck disable=SC2016  # backticks are literal text in the message
				printf '  note: %s is BELOW its floor (%s%% < %s%%) and passing only on tolerance — re-record with `make cover-update`\n' \
					"$dir" "$now" "$floor"
			fi
		fi
	done <"$BASELINE_FILE"

	# Warn (do not fail) when a module's coverage has risen well above its floor:
	# a good moment to ratchet the baseline up with `make cover-update`.
	while IFS=$'\t' read -r dir now _; do
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

main() {
	# Serialize against a concurrent heavy build (queue rather than saturate
	# cores) — the same desktop-safety make test has — so make check can fold in
	# the coverage ratchet without a second, unthrottled test pass. No-op on
	# CI/headless. Must run before anything else: it re-execs this script.
	serialize_heavy_build "$@"

	# All coverage temp files live in a per-run directory under the repo-local,
	# gitignored tmp/ — never the host-wide $TMPDIR (/var/folders on macOS, /tmp
	# elsewhere). Host-wide temp is shared across worktrees and sessions, so
	# concurrent runs collide on the fixed `cover.XXXXXX.*` template; a per-run
	# directory under this worktree's tmp/ keeps every run's scratch isolated (the
	# same reason #615 moved the e2e JUnit report here). The trap removes the
	# directory on any exit — including SIGTERM/SIGINT from an interrupted `make
	# check` timeout — so a killed run can't strand `cover.XXXXXX.out` files that
	# would make the next `mktemp` fail (`File exists`) and report spurious "no
	# coverage" regressions.
	mkdir -p "$REPO_ROOT/tmp"
	# Best-effort sweep of any cover.* run directory left by a prior run that was
	# SIGKILLed or crashed before its trap could fire (SIGTERM is handled by the
	# trap below). Safe here: serialize_heavy_build has returned, so no concurrent
	# same-worktree coverage run is holding one, and each run's directory is
	# uniquely named so this can never touch a live sibling.
	rm -rf "$REPO_ROOT"/tmp/cover.* 2>/dev/null || true
	RUN_TMP="$(mktemp -d "$REPO_ROOT/tmp/cover.XXXXXX")"
	cleanup() { rm -rf "$RUN_TMP" 2>/dev/null || true; }
	trap cleanup EXIT
	trap 'cleanup; exit 130' INT
	trap 'cleanup; exit 143' TERM

	case "${1:-report}" in
		report) cmd_report ;;
		update) cmd_update ;;
		check)  cmd_check ;;
		*) echo "usage: $0 {report|update|check}" >&2; exit 2 ;;
	esac
}

# Run main only when executed directly, so coverage-test.sh can source this file
# and exercise the pure profile-splitting helpers without running the suite.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
