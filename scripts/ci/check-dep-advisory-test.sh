#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-dep-advisory.sh — the `make check` dependency
# reminder. The script decides, from git state alone, whether a change touches a
# dependency file CI gates separately; that classification is asserted here in a
# hermetic throwaway repo so no assumption about the caller's tree leaks in. Runs
# under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/ci/check-dep-advisory.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# A throwaway repo with an origin/main to exercise the branch-diff source. Git
# identity is set locally (never --global) so the harness branch-guard is happy.
setup_repo() {
	local d="$WORKDIR/repo"
	rm -rf "$d"
	mkdir -p "$d"
	(
		cd "$d"
		git init -q -b main
		# Q820: no detached maintenance racing the next command in a fixture repo.
		git config maintenance.auto false
		git config user.email t@t.t
		git config user.name t
		# A base commit on `main`, then a clone-like `origin/main` ref pointing at
		# it, so `origin/main...HEAD` resolves without a network remote.
		printf 'seed\n' >README.md
		mkdir -p sub vendor
		printf 'module x\n\ngo 1.26.5\n' >sub/go.mod
		printf 'module m\n\ngo 1.26.5\n' >go.mod
		git add -A
		git commit -qm base
		git update-ref refs/remotes/origin/main HEAD
	)
	printf '%s\n' "$d"
}

# expect NAME WANT_STATE  (callback mutates the repo before the run)
# WANT_STATE is "advise" (script prints + names files) or "silent" (no output).
# The callback runs inside the repo dir; on return we run the script there.
expect() {
	local name="$1" want="$2" mutate="$3" d out got
	d="$(setup_repo)"
	( cd "$d" && eval "$mutate" )
	out="$(cd "$d" && "$SCRIPT" 2>&1)"
	if [[ -n "$out" ]]; then got=advise; else got=silent; fi
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want=%s got=%s\n%s\n' "$name" "$want" "$got" "$out" >&2
		fails=$((fails + 1))
	fi
}

# Working-tree edits.
expect 'unstaged go.mod edit          -> advise' advise 'printf x >>go.mod'
expect 'unstaged nested go.mod edit   -> advise' advise 'printf x >>sub/go.mod'
expect 'unstaged vendor/ edit         -> advise' advise 'printf x >>vendor/modules.txt'
expect 'unstaged non-dep edit         -> silent' silent 'printf x >>README.md'
expect 'clean tree                    -> silent' silent 'true'

# Staged (but uncommitted) edits.
expect 'staged go.mod edit            -> advise' advise 'printf x >>go.mod; git add go.mod'

# Committed on this branch, clean tree — the 586 scenario the reminder targets.
expect 'committed dep change vs base  -> advise' advise \
	'printf "\nx\n" >>go.mod; git commit -qam c'
expect 'committed non-dep change      -> silent' silent \
	'printf "\nx\n" >>README.md; git commit -qam c'

# A new go.work / go.work.sum / THIRD-PARTY-NOTICES / go.work.gen also counts.
expect 'new go.work.sum               -> advise' advise 'printf h >go.work.sum; git add go.work.sum'
expect 'new go.work.gen               -> advise' advise 'printf g >sub/go.work.gen; git add sub/go.work.gen'

# Robustness: outside a git repo, the script must stay silent and succeed.
notgit="$WORKDIR/notgit"
mkdir -p "$notgit"
out="$(cd "$notgit" && "$SCRIPT" 2>&1)"; rc=$?
if [[ -z "$out" && "$rc" == 0 ]]; then
	printf 'ok   outside a git repo          -> silent, rc=0\n'
else
	printf 'FAIL outside a git repo: rc=%s out=%q\n' "$rc" "$out" >&2
	fails=$((fails + 1))
fi

# It must NEVER exit non-zero, even when advising — it is `make check`'s last step.
d="$(setup_repo)"; ( cd "$d" && printf x >>go.mod )
( cd "$d" && "$SCRIPT" >/dev/null 2>&1 ); rc=$?
if [[ "$rc" == 0 ]]; then
	printf 'ok   advising still exits 0\n'
else
	printf 'FAIL advising exited %s (must be 0 — it is the last step of make check)\n' "$rc" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\ncheck-dep-advisory-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-dep-advisory-test: ok\n'
