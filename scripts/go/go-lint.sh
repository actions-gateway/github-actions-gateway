#!/usr/bin/env bash
#
# Lint the Go workspace: a gofmt formatting check across every go.work module,
# then golangci-lint (which includes govet) per module. Backs `make lint` and
# the `lint` job in .github/workflows/unit-test.yml.
#
# Local runs scope golangci-lint to the modules a change can actually affect:
# the modules owning files changed vs the origin/main merge-base (committed,
# uncommitted, and untracked), plus every module that depends on one of them,
# transitively — a dependency change can break a dependent's typecheck and
# therefore its lint. Every other module lints byte-identically
# to the merge-base commit (same sources, same .golangci.yml, same
# tools/-pinned linter), and that commit is on main where the CI full sweep
# already ran green, so re-linting it proves nothing. Skipping it saves real
# time — most branches touch one or two modules — and releases the
# machine-wide heavy-build lock sooner for sibling worktree sessions.
#
# The full per-module sweep still runs whenever scoping can't be trusted or is
# asked for: on CI (the authority), with LINT_ALL=1, when no origin/main
# merge-base resolves, or when the change touches anything with
# workspace-wide effect (go.work*, .golangci.yml, vendor/, tools/, this
# script and its helpers). The gofmt check always covers every module — it
# costs seconds.
#
# Env:
#   GOLANGCI_LINT  Path to the golangci-lint binary (default .build/golangci-lint
#                  at the repo root — build it with `make golangci-lint`).
#   GOLANGCI_LINT_CACHE  Analysis cache dir; respected when set. Otherwise
#                  local runs use the worktree's own tmp/golangci-lint and CI
#                  keeps golangci-lint's user-level default — see lint_cache_dir.
#   LINT_ALL=1     Force the full per-module sweep on a local run.
#
# Applies the local throttle (GOMAXPROCS + `-j` cap and a low-priority QoS
# prefix) on a GUI dev shell; a no-op on CI/headless — see
# scripts/agent/local-throttle.sh. golangci-lint ignores GOMAXPROCS, so the `-j`
# flag is the lever that actually caps its fan-out.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

GOLANGCI_LINT="${GOLANGCI_LINT:-$REPO_ROOT/.build/golangci-lint}"

# --- change-scoping helpers (pure — asserted by go-lint-scope-test.sh) -------

# in_list NEEDLE [WORD...] — succeed when NEEDLE equals one of the WORDs.
in_list() {
	local needle="$1" word
	shift
	for word in "$@"; do
		[[ "$word" == "$needle" ]] && return 0
	done
	return 1
}

# is_workspace_wide FILE — succeed when a change to FILE can shift lint results
# in ANY module, putting scoping off the table: the workspace/vendor state, the
# shared lint config, the tools/-pinned linter version, and this scoping
# machinery itself.
is_workspace_wide() {
	local file="$1"
	case "$file" in
	go.work | go.work.sum | .golangci.yml | \
		scripts/go/go-lint.sh | scripts/lib/common.sh | scripts/agent/local-throttle.sh | \
		vendor/* | tools/*)
		return 0
		;;
	esac
	return 1
}

# owning_module FILE MODULES — print the module dir (from the newline-separated
# MODULES list) that contains FILE; the longest match wins so a nested module
# beats its parent. Prints nothing when no module owns FILE.
owning_module() {
	local file="$1" modules="$2" m best=""
	while IFS= read -r m; do
		[[ "$file" == "$m"/* ]] || continue
		(( ${#m} > ${#best} )) && best="$m"
	done <<<"$modules"
	[[ -n "$best" ]] && printf '%s\n' "$best"
	return 0
}

# lint_cache_dir REPO_ROOT CI EXPLICIT — print the analysis cache dir to use,
# or nothing to leave the environment alone. Local runs get a per-worktree dir:
# the user-level default is shared across worktrees and a cached entry keeps
# the absolute path of the worktree that produced it, so a hit produced by a
# since-deleted sibling makes the postprocessors report phantom findings
# (Q516). Sharing is cheap to lose — off a warm GOCACHE the analysis re-runs
# in ~1 s per module. CI keeps the default (runners are fresh, and
# unit-test.yml caches that path); an explicit GOLANGCI_LINT_CACHE wins.
lint_cache_dir() {
	local repo_root="$1" ci="$2" explicit="$3"
	if [[ -n "$ci" || -n "$explicit" ]]; then
		return 0
	fi
	printf '%s/tmp/golangci-lint\n' "$repo_root"
}

# lint_scope MODULES EDGES — decide which modules golangci-lint must cover for
# a change set.
#
#   stdin:   changed file paths, repo-relative, one per line
#   MODULES: first-party module dirs, newline-separated, repo-relative
#   EDGES:   module dependencies, newline-separated "dependent dependency"
#
# Prints one line: "full <file>" when a workspace-wide file changed, else
# "modules[ <dir>...]" — the changed modules plus every module that depends on
# one of them, transitively, in MODULES order. Files owned by no module
# (docs/, charts/, ...) cannot alter Go lint results and are ignored.
lint_scope() {
	local modules="$1" edges="$2"
	local file m affected=()
	while IFS= read -r file; do
		[[ -z "$file" ]] && continue
		if is_workspace_wide "$file"; then
			# Drain the rest of stdin before returning: an early exit would
			# SIGPIPE the git producers feeding the pipe in compute_scope, and
			# under pipefail that fails the whole scope computation.
			cat >/dev/null
			printf 'full %s\n' "$file"
			return 0
		fi
		m="$(owning_module "$file" "$modules")"
		if [[ -n "$m" ]] && ! in_list "$m" "${affected[@]-}"; then
			affected+=("$m")
		fi
	done
	# Pull in dependents until the set stops growing (handles chains like
	# githubapp -> broker -> agc even without a direct edge).
	local grew=1 dependent dependency
	while (( grew )); do
		grew=0
		while read -r dependent dependency; do
			[[ -z "$dependent" ]] && continue
			if in_list "$dependency" "${affected[@]-}" &&
				! in_list "$dependent" "${affected[@]-}"; then
				affected+=("$dependent")
				grew=1
			fi
		done <<<"$edges"
	done
	printf 'modules'
	while IFS= read -r m; do
		if in_list "$m" "${affected[@]-}"; then
			printf ' %s' "$m"
		fi
	done <<<"$modules"
	printf '\n'
}

# --- change-scoping inputs (git/go state) ------------------------------------

# module_dirs — workspace module dirs, newline-separated, repo-relative
# without the ./ prefix go.work uses.
module_dirs() {
	local m
	while IFS= read -r m; do
		printf '%s\n' "${m#./}"
	done < <(workspace_modules)
}

# scoped_module_dirs — every first-party module dir change-scoping must be able
# to name: the go.work members plus the modules outside the workspace.
#
# Deriving this from go.work alone is what disarmed the gate in Q670: a branch
# adding six files under devtools/ owned no workspace module, so lint_scope
# printed an empty module set, main() reported "no module changes" and returned
# 0 — a green that had linted nothing, while CI's full sweep found twelve. Both
# halves are discovered (go.work, then every other tracked go.mod), so a module
# added later is scoped without editing this script.
scoped_module_dirs() {
	module_dirs
	firstparty_nonworkspace_modules
}

# module_edges MODULES — print one "dependent dependency" line per
# workspace-local replace directive. The replace directives are the
# workspace's own dependency declarations: every module that imports a
# sibling carries one pointing at its relative path.
module_edges() {
	local modules="$1" m rel dep
	while IFS= read -r m; do
		while IFS= read -r rel; do
			[[ "$rel" == ./* || "$rel" == ../* ]] || continue
			# A replace pointing at a missing dir would break the build anyway;
			# don't let it crash the scope computation.
			if ! dep="$(cd "$m/$rel" 2>/dev/null && pwd)"; then
				continue
			fi
			printf '%s %s\n' "$m" "${dep#"$REPO_ROOT"/}"
		done < <(go mod edit -json "$m/go.mod" | jq -r '.Replace[]?.New.Path')
	done <<<"$modules"
}

# compute_scope — print the lint_scope decision line for the current checkout.
# Fails open to the full sweep whenever scoping doesn't apply (CI, LINT_ALL=1)
# or its inputs are unavailable (no origin/main merge-base).
compute_scope() {
	if [[ -n "${CI:-}" ]]; then
		printf 'full CI\n'
		return 0
	fi
	if [[ -n "${LINT_ALL:-}" ]]; then
		printf 'full LINT_ALL=1\n'
		return 0
	fi
	local base
	if ! base="$(git merge-base HEAD origin/main 2>/dev/null)" || [[ -z "$base" ]]; then
		printf 'full no origin/main merge-base\n'
		return 0
	fi
	local modules edges
	modules="$(scoped_module_dirs)"
	edges="$(module_edges "$modules")"
	{
		git diff --name-only "$base" --
		git ls-files --others --exclude-standard
	} | lint_scope "$modules" "$edges"
}

# --- lint --------------------------------------------------------------------

# lint_module DIR J_FLAG GOWORK — run golangci-lint in DIR. GOWORK is "off" for a
# module outside go.work, empty otherwise.
lint_module() {
	local dir="$1" j_flag="$2" gowork="$3"
	echo "==> golangci-lint $dir${gowork:+ (GOWORK=$gowork)}"
	(
		cd "$dir"
		[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
		[[ -n "$gowork" ]] && export GOWORK="$gowork"
		# shellcheck disable=SC2086  # flag string and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX "$GOLANGCI_LINT" run $j_flag --config "$REPO_ROOT/.golangci.yml" ./...
	)
}

main() {
	local unformatted gofmt_dirs dir
	gofmt_dirs="$(workspace_modules)"
	# A non-workspace module vendors inside its own tree, and vendored
	# third-party source is not gofmt-clean. `go list` yields the module's own
	# package dirs and nothing under vendor/. Workspace modules need no such
	# filter: their vendor tree is the shared one at the repo root.
	for dir in $(firstparty_nonworkspace_modules); do
		gofmt_dirs+=" $(cd "$dir" && GOWORK=off go list -f '{{.Dir}}' ./...)"
	done
	# shellcheck disable=SC2086  # module paths word-split intentionally (no spaces in go.work paths)
	unformatted="$(gofmt -l $gofmt_dirs)"
	if [[ -n "$unformatted" ]]; then
		echo "gofmt: the following files are not formatted:"
		echo "$unformatted"
		echo "run: gofmt -w <file>"
		exit 1
	fi

	local scope kind lint_dirs nonworkspace
	nonworkspace="$(firstparty_nonworkspace_modules)"
	scope="$(compute_scope)"
	kind="${scope%% *}"
	if [[ "$kind" == full ]]; then
		lint_dirs="$(scoped_module_dirs)"
	else
		lint_dirs="${scope#modules}"
		if [[ -z "${lint_dirs// /}" ]]; then
			echo "==> lint scope: no module changes vs the origin/main merge-base — golangci-lint skipped (LINT_ALL=1 forces the full sweep)"
			return 0
		fi
	fi

	# Serialize against a concurrent heavy build on this machine (no-op on
	# CI/headless) so two sessions don't saturate the cores and push
	# golangci-lint past its deadline; re-execs self under a machine-wide lock.
	# Taken only now, after gofmt and the scope decision: a run with nothing to
	# lint never queues behind a sibling's heavy phase. (The re-exec'd child
	# reruns everything above — deterministic, cheap, and silent on success —
	# so the scope line below prints once, from the lock holder.)
	serialize_heavy_build "$@"

	if [[ "$kind" == full ]]; then
		echo "==> lint scope: full sweep — ${scope#full }"
	else
		echo "==> lint scope:${lint_dirs} — changed vs the origin/main merge-base, plus dependents (LINT_ALL=1 for the full sweep)"
	fi

	if [[ ! -x "$GOLANGCI_LINT" ]]; then
		echo "golangci-lint not found at $GOLANGCI_LINT — build it with: make golangci-lint" >&2
		exit 1
	fi

	local cache_dir
	cache_dir="$(lint_cache_dir "$REPO_ROOT" "${CI:-}" "${GOLANGCI_LINT_CACHE:-}")"
	if [[ -n "$cache_dir" ]]; then
		export GOLANGCI_LINT_CACHE="$cache_dir"
	fi

	init_throttle
	local j_flag=""
	[[ -n "$THROTTLE_JOBS" ]] && j_flag="-j $THROTTLE_JOBS"

	local dir
	# shellcheck disable=SC2086  # lint_dirs word-splits intentionally (no spaces in module paths)
	for dir in $lint_dirs; do
		# A module go.work does not list resolves nothing through the workspace,
		# so it lints against its own vendor tree with GOWORK=off.
		# shellcheck disable=SC2086  # nonworkspace word-splits into in_list's WORD args
		if in_list "$dir" $nonworkspace; then
			lint_module "$dir" "$j_flag" off
		else
			lint_module "$dir" "$j_flag" ""
		fi
	done
}

# Run main only when executed directly, so go-lint-scope-test.sh can source
# this file to exercise the pure scoping helpers without linting anything.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
