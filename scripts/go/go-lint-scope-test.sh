#!/usr/bin/env bash
#
# Unit tests for the pure helpers in scripts/go/go-lint.sh: lint_scope
# (workspace-wide full-sweep triggers, file→module ownership, transitive
# dependent expansion), owning_module (longest-match rule), and lint_cache_dir
# (per-worktree analysis cache scoping). These decide which modules
# golangci-lint covers on a local run and where its cache lives, so they are
# asserted here without invoking the linter. Runs under `make check` (via
# `make scripts-test`) and the CI shellcheck job.
#
# Fixture assertions come first, then a block run against the checkout's own
# module graph — a scoping input that names the wrong module set looks correct
# to every fixture (Q670).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its functions; the BASH_SOURCE guard there
# keeps main() from running on source.
# shellcheck source=scripts/go/go-lint.sh
source "$REPO_ROOT/scripts/go/go-lint.sh"

fails=0

# A synthetic workspace mirroring the real shape: a shared leaf (githubapp), a
# mid-layer (broker), and two controllers where one depends on the other.
MODULES=$'api\nbroker\ncmd/agc\ncmd/gmc\ngithubapp'
# "dependent dependency" — note there is NO direct cmd/gmc->broker or
# cmd/agc->githubapp edge, so those closures must come out of the fixed point.
EDGES=$'broker githubapp\ncmd/agc api\ncmd/agc broker\ncmd/gmc api\ncmd/gmc cmd/agc'

# expect_scope NAME CHANGED_FILES WANT — feed newline-separated changed paths
# to lint_scope and assert its decision line matches WANT exactly.
expect_scope() {
	local name="$1" files="$2" want="$3" got
	got="$(lint_scope "$MODULES" "$EDGES" <<<"$files")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   scope %-24s -> %s\n' "$name" "$got"
	else
		printf 'FAIL scope %-24s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# No Go module touched: nothing to lint.
expect_scope docs-only $'docs/STATUS.md\nREADME.md' 'modules'
expect_scope empty-input '' 'modules'
expect_scope non-module-go-file 'hack/tool.go' 'modules'
# A module-name prefix must not match without the / boundary (apifoo != api/).
expect_scope prefix-no-boundary 'apifoo/main.go' 'modules'

# Single-module change with no dependents: just that module.
expect_scope leaf-module 'cmd/gmc/main.go' 'modules cmd/gmc'
# Direct dependents: api is imported by both controllers.
expect_scope direct-dependents 'api/v2/types.go' 'modules api cmd/agc cmd/gmc'
# Transitive closure: githubapp -> broker -> cmd/agc -> cmd/gmc, with no
# direct edge past broker.
expect_scope transitive-dependents 'githubapp/app.go' 'modules broker cmd/agc cmd/gmc githubapp'
# Module files mixed with non-module files: only the module counts, once.
expect_scope mixed-with-docs $'docs/a.md\nbroker/b.go\nbroker/c.go' 'modules broker cmd/agc cmd/gmc'

# Workspace-wide triggers force the full sweep, whatever else changed.
expect_scope trigger-go-work $'api/x.go\ngo.work' 'full go.work'
expect_scope trigger-lint-config '.golangci.yml' 'full .golangci.yml'
expect_scope trigger-vendor 'vendor/foo/bar.go' 'full vendor/foo/bar.go'
expect_scope trigger-tools 'tools/go.mod' 'full tools/go.mod'
expect_scope trigger-self 'scripts/go/go-lint.sh' 'full scripts/go/go-lint.sh'
expect_scope trigger-throttle 'scripts/agent/local-throttle.sh' 'full scripts/agent/local-throttle.sh'
# ...but a same-named file deeper in the tree is NOT a trigger — it scopes to
# its owning module (plus cmd/gmc, which depends on cmd/agc).
expect_scope no-trigger-lookalike 'cmd/agc/testdata/go.work' 'modules cmd/agc cmd/gmc'

# Regression: lint_scope must drain stdin when a trigger short-circuits it.
# An early return SIGPIPEs the producers feeding the pipe once the input
# outgrows the pipe buffer, and under pipefail that fails the whole scope
# computation (observed as exit 141 from compute_scope).
drain_scope() {
	{
		printf 'go.work\n'
		seq 1 200000 | awk '{print "docs/f" $1 ".md"}'
	} | lint_scope "$MODULES" "$EDGES"
}
if got="$(drain_scope)" && [[ "$got" == 'full go.work' ]]; then
	printf 'ok   scope %-24s -> %s\n' drain-stdin-on-trigger "$got"
else
	printf 'FAIL scope %-24s want=[full go.work] got=[%s] (pipeline rc matters too)\n' \
		drain-stdin-on-trigger "${got:-}" >&2
	fails=$((fails + 1))
fi

# expect_owner NAME FILE MODULES WANT — assert owning_module's longest-match.
expect_owner() {
	local name="$1" file="$2" modules="$3" want="$4" got
	got="$(owning_module "$file" "$modules")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   owner %-24s -> [%s]\n' "$name" "$got"
	else
		printf 'FAIL owner %-24s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# Longest match wins when one module dir nests inside another.
expect_owner nested-longest-match 'cmd/agc/main.go' $'cmd\ncmd/agc' 'cmd/agc'
expect_owner nested-parent-owns 'cmd/other/main.go' $'cmd\ncmd/agc' 'cmd'
expect_owner no-owner 'docs/index.md' $'cmd\ncmd/agc' ''

# expect_cache NAME REPO_ROOT CI EXPLICIT WANT — assert lint_cache_dir's
# decision: a per-worktree dir locally, nothing on CI or an explicit override.
expect_cache() {
	local name="$1" want="$5" got
	got="$(lint_cache_dir "$2" "$3" "$4")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   cache %-24s -> [%s]\n' "$name" "$got"
	else
		printf 'FAIL cache %-24s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect_cache local-per-worktree '/wt/a' '' '' '/wt/a/tmp/golangci-lint'
expect_cache ci-keeps-default '/wt/a' 'true' '' ''
expect_cache explicit-wins '/wt/a' '' '/custom/cache' ''

# --- the real module graph ---------------------------------------------------
#
# The fixtures above cannot catch a scoping input that names the wrong set of
# modules, and that is the failure this gate has actually shipped: scoping fed
# on go.work alone mapped a devtools-only branch to no module at all, so the
# gate printed "no module changes", skipped golangci-lint and exited 0 while
# twelve findings sat in the diff (Q670). These assertions run against the
# checkout's own module graph, so they go red the moment a first-party module
# stops being scoped — including one added after this test was written.
REAL_MODULES="$(scoped_module_dirs)"
REAL_EDGES="$(module_edges "$REAL_MODULES")"

# expect_real NAME CHANGED_FILES WANT — as expect_scope, against the real graph.
expect_real() {
	local name="$1" files="$2" want="$3" got
	got="$(lint_scope "$REAL_MODULES" "$REAL_EDGES" <<<"$files")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   real  %-24s -> %s\n' "$name" "$got"
	else
		printf 'FAIL real  %-24s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# Every non-workspace module must be scopable. Asserted per module rather than
# by naming devtools, so the next one is covered without editing this file.
while IFS= read -r nonws; do
	[[ -n "$nonws" ]] || continue
	expect_real "nonworkspace-$nonws" "$nonws/probe.go" "modules $nonws"
done < <(firstparty_nonworkspace_modules)

# Controls — scoping must still narrow, not degrade to "lint everything".
# A workspace module scopes to itself plus its dependents, and pulls in no
# non-workspace module (they import nothing from the workspace).
expect_real workspace-module-alone 'cmd/agc/main.go' 'modules cmd/agc cmd/gmc'
# No Go module touched: still nothing to lint.
expect_real no-go-change 'docs/STATUS.md' 'modules'

if (( fails > 0 )); then
	echo "go-lint-scope-test: $fails failure(s)" >&2
	exit 1
fi
echo "go-lint-scope-test: all assertions passed"
