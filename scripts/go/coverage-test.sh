#!/usr/bin/env bash
#
# Unit tests for the pure profile-splitting helpers in scripts/go/coverage.sh:
# module_import_path (go.mod -> import path) and module_profile (merged
# workspace profile -> one module's slice, with the excluded files dropped).
#
# These decide the number every module's ratchet floor is compared against, and
# the split replaced a per-module `go test` that could not get the attribution
# wrong by construction — so the boundary and exclusion rules are asserted here
# rather than left to a full coverage run to notice. Runs under `make check`
# (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its functions; the BASH_SOURCE guard there
# keeps main() from running on source.
# shellcheck source=scripts/go/coverage.sh
source "$REPO_ROOT/scripts/go/coverage.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# A merged profile in the shape `go test -coverprofile` emits for a
# workspace-wide run: package paths, not file paths, one line per block.
PROFILE="$WORKDIR/merged.out"
cat >"$PROFILE" <<'EOF'
mode: set
example.com/repo/agc/internal/foo/a.go:1.1,2.2 1 1
example.com/repo/agc/api/v1/zz_generated.deepcopy.go:1.1,2.2 1 1
example.com/repo/agc/api/v1/groupversion_info.go:1.1,2.2 1 0
example.com/repo/agc/test/load/harness.go:1.1,2.2 1 1
example.com/repo/agcutil/b.go:1.1,2.2 1 1
example.com/repo/broker/broker.go:1.1,2.2 1 1
example.com/repo/broker/brokertest/stub.go:1.1,2.2 1 1
example.com/repo/broker/brokerstub/core.go:1.1,2.2 1 1
example.com/repo/api/groupversion_info.go:1.1,2.2 1 1
example.com/repo/test/fakegithub/fake.go:1.1,2.2 1 1
EOF

# expect_slice NAME MODPATH WANT — assert module_profile emits exactly WANT
# (newline-separated, header included).
expect_slice() {
	local name="$1" modpath="$2" want="$3" got
	got="$(module_profile "$modpath" "$PROFILE")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   slice %-22s -> %d line(s)\n' "$name" "$(wc -l <<<"$got" | tr -d ' ')"
	else
		printf 'FAIL slice %-22s\nwant=[%s]\ngot =[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# A module claims its own packages and nothing else: generated DeepCopy code,
# scheme-registration boilerplate and the `test/` helper tree are dropped, and
# the sibling module whose path merely shares the prefix stays out.
expect_slice agc example.com/repo/agc \
	$'mode: set\nexample.com/repo/agc/internal/foo/a.go:1.1,2.2 1 1'

# The `/` boundary in the other direction: agcutil owns only its own line even
# though `agc` sorts as a prefix of it.
expect_slice prefix-sibling example.com/repo/agcutil \
	$'mode: set\nexample.com/repo/agcutil/b.go:1.1,2.2 1 1'

# Both helper conventions are excluded — `<pkg>test` (Q110) and the `<pkg>stub`
# protocol model it is built on (Q528) — while the production package is not.
expect_slice pkgtest-helper example.com/repo/broker \
	$'mode: set\nexample.com/repo/broker/broker.go:1.1,2.2 1 1'

# Header-only results are the "n/a" signal measure_all keys off: a module whose
# every profiled file is excluded, and one with no profiled lines at all.
expect_slice all-excluded example.com/repo/api 'mode: set'
expect_slice test-tree-module example.com/repo/test/fakegithub 'mode: set'
expect_slice no-lines example.com/repo/absent 'mode: set'

# expect_stmts NAME PROFILE WANT — assert profile_statements totals WANT.
expect_stmts() {
	local name="$1" prof="$2" want="$3" got
	got="$(profile_statements "$prof")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   statements %-18s -> %s\n' "$name" "$got"
	else
		printf 'FAIL statements %-18s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# The denominator is per distinct BLOCK, not per row. A workspace-wide profile
# writes a block once per instrumented test binary that reported it, so an
# uncached package repeats and a cached one does not — summing rows sizes the
# tolerance off the test cache rather than the tree (Q989). Same blocks, two
# multiplicities, one answer.
SINGLE="$WORKDIR/single.out"
cat >"$SINGLE" <<'EOF'
mode: set
example.com/repo/m/a.go:1.1,2.2 3 1
example.com/repo/m/a.go:4.1,6.2 2 0
example.com/repo/m/b.go:1.1,9.9 5 1
EOF

# The repeats agree on NumStmt and disagree on the hit count, which is the
# shape `go tool cover -func` merges: it sums the counts and counts the
# statements once.
REPEATED="$WORKDIR/repeated.out"
cat >"$REPEATED" <<'EOF'
mode: set
example.com/repo/m/a.go:1.1,2.2 3 1
example.com/repo/m/a.go:4.1,6.2 2 0
example.com/repo/m/b.go:1.1,9.9 5 1
example.com/repo/m/a.go:1.1,2.2 3 1
example.com/repo/m/a.go:4.1,6.2 2 1
example.com/repo/m/b.go:1.1,9.9 5 0
example.com/repo/m/a.go:1.1,2.2 3 0
EOF

expect_stmts single-rows "$SINGLE" 10
expect_stmts repeated-rows "$REPEATED" 10
printf 'mode: set\n' >"$WORKDIR/stmts-header-only.out"
expect_stmts header-only "$WORKDIR/stmts-header-only.out" 0

# The other direction: module_profile must NOT drop the repeats. `go tool cover
# -func` sums their counts, so a block one binary covered and another missed is
# covered; deduping the rows here would drop that hit and move the percentage.
got="$(module_profile example.com/repo/m "$REPEATED" | grep -c 'a.go:4\.1,6\.2' || true)"
if [[ "$got" == "2" ]]; then
	printf 'ok   slice %-22s -> repeats kept (%s rows)\n' keeps-repeats "$got"
else
	printf 'FAIL slice %-22s want=[2] got=[%s]\n' keeps-repeats "$got" >&2
	fails=$((fails + 1))
fi

# module_import_path reads the `module` directive and stops there — a `module`
# token appearing later (in a require block comment, say) must not win.
mkdir -p "$WORKDIR/mod"
cat >"$WORKDIR/mod/go.mod" <<'EOF'
module example.com/repo/agc

go 1.25

require example.com/other v1.2.3 // the other module
EOF
got_path="$(module_import_path "$WORKDIR/mod")"
if [[ "$got_path" == "example.com/repo/agc" ]]; then
	printf 'ok   module-path            -> %s\n' "$got_path"
else
	printf 'FAIL module-path            want=[example.com/repo/agc] got=[%s]\n' "$got_path" >&2
	fails=$((fails + 1))
fi

# expect_tol NAME NSTMT WANT — assert effective_tolerance NSTMT rounds to WANT pp.
expect_tol() {
	local name="$1" nstmt="$2" want="$3" got
	got="$(printf '%.2f' "$(effective_tolerance "$nstmt")")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   tolerance %-20s -> %spp\n' "$name" "$got"
	else
		printf 'FAIL tolerance %-20s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# The tolerance is the larger of the fixed pp floor and TOLERANCE_STMT statements,
# and BOTH directions matter: a rule that always took the statement figure would
# tighten every large module (0.17pp on 1800 statements), and one that always took
# the fixed figure is the Q803 defect. With the defaults (0.5pp, 3 statements) the
# crossover is at 600 statements.
expect_tol large-module-fixed-pp 1800 0.50   # 3 stmt = 0.17pp, so 0.5pp wins
expect_tol crossover 600 0.50                # 3 stmt = 0.50pp exactly
expect_tol small-module-statements 348 0.86  # a small module: 3 stmt = 0.86pp wins
expect_tol tiny-module 100 3.00              # 3 stmt = 3pp

# What the Q989 denominator was worth: cmd/proxy measured 386 statements on
# 2026-09-04, and the same module counted once per reporting test binary reads
# 772, which crosses the 600-statement boundary and hands it the fixed floor.
expect_tol proxy-per-block 386 0.78          # 3 stmt = 0.78pp wins
expect_tol proxy-per-row 772 0.50            # 3 stmt = 0.39pp, so 0.5pp wins

# A module with no measurable coverage reports 0 statements; dividing by that
# denominator must not produce inf/NaN and silently disable the gate.
expect_tol no-statements 0 0.50
expect_tol non-numeric n/a 0.50

# Every go.work module must actually declare a module path, or the split would
# silently attribute its packages to nothing and report a false "n/a".
while read -r dir; do
	path="$(module_import_path "$dir")"
	if [[ -n "$path" ]]; then
		printf 'ok   workspace-module       %-20s -> %s\n' "$dir" "$path"
	else
		printf 'FAIL workspace-module       %s declares no module path\n' "$dir" >&2
		fails=$((fails + 1))
	fi
done < <(workspace_modules)

# A module outside go.work is measured and ratcheted like any other, so every one
# must carry a baseline row. Without this assertion, adding a non-workspace
# module leaves its packages unratcheted and nothing says so: the tests still
# run, so a failing one is still caught, and only a deletion or a slide goes
# unseen.
while read -r dir; do
	if grep -qE "^\./${dir}[[:space:]]" coverage-baseline.txt; then
		printf 'ok   nonworkspace-module    %-20s has a baseline row\n' "./$dir"
	else
		printf 'FAIL nonworkspace-module    %s has no row in coverage-baseline.txt\n' "./$dir" >&2
		fails=$((fails + 1))
	fi
done < <(firstparty_nonworkspace_modules)

# report_module must report n/a rather than a number it cannot defend: a
# header-only profile and a missing one both mean nothing was measured, and a
# bogus 0.0% floor there would gate on noise.
printf 'mode: set\n' >"$WORKDIR/header-only.out"
got="$(report_module ./x "$WORKDIR/header-only.out" "")"
if [[ "$got" == $'./x\tn/a\t0' ]]; then
	printf 'ok   report-module          header-only profile -> n/a\n'
else
	printf 'FAIL report-module          header-only profile: got %q\n' "$got" >&2
	fails=$((fails + 1))
fi

got="$(report_module ./x "$WORKDIR/does-not-exist.out" "")"
if [[ "$got" == $'./x\tn/a\t0' ]]; then
	printf 'ok   report-module          missing profile     -> n/a\n'
else
	printf 'FAIL report-module          missing profile: got %q\n' "$got" >&2
	fails=$((fails + 1))
fi

if (( fails > 0 )); then
	printf '\n%d coverage-split assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nAll coverage-split assertions passed.\n'
