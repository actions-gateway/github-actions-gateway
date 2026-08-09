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

if (( fails > 0 )); then
	printf '\n%d coverage-split assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nAll coverage-split assertions passed.\n'
