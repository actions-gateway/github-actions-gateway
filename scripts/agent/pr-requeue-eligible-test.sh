#!/usr/bin/env bash
#
# Unit tests for scripts/agent/pr-requeue-eligible.sh (Q692).
#
# This gate decides whether a session may merge something unattended, so the
# assertions that matter are the refusals: a PR no human ever enqueued, one a
# bot enqueued, one already in the queue, and — the case the maintainer chose
# the policy around — a rebase that resolves a conflict outside the files the
# merge drivers own. Each is built as a real defect and required to come back
# WAKE, because a checker that silently returns ELIGIBLE looks exactly like one
# that is working.
#
# The conflicts are real: each case builds a throwaway git repo and diverges two
# branches, so `merge-tree` does the same work it does in anger. `gh` is stubbed
# (it answers with what the real command prints after its own --jq), which means
# these assertions do not cover the jq expressions themselves — that half is
# exercised by running the script against a live PR.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECKER="$REPO_ROOT/scripts/agent/pr-requeue-eligible.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/pr-requeue-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

# A `gh` stub that answers from the environment. Each branch prints exactly what
# the real invocation prints after gh's own --jq has run.
cat >"$FIXTURE_DIR/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$1 ${2-}" in
"pr view")
	printf '%s\t%s\t%s\n' "${GH_STATE:-OPEN}" "${GH_DRAFT:-false}" "${GH_BASE:-main}"
	;;
"api graphql")
	printf '%s\n' "${GH_QUEUE_ENTRY:-}"
	;;
"api "*)
	printf '%s\n' "${GH_ENQUEUE_COUNT:-0}"
	;;
*)
	printf 'gh stub: unhandled: %s\n' "$*" >&2
	exit 1
	;;
esac
STUB
chmod +x "$FIXTURE_DIR/bin/gh"
export PATH="$FIXTURE_DIR/bin:$PATH"

fails=0
LAST_OUT=""

# new_repo NAME CONFLICT_KIND — a throwaway repo whose HEAD and origin/main have
# diverged. CONFLICT_KIND picks which file both sides edited: status (a
# driver-owned file), code (not), both, or none.
new_repo() {
	local name="$1" kind="$2"
	local dir="$FIXTURE_DIR/$name"
	mkdir -p "$dir/docs/plan" "$dir/scripts"
	(
		cd "$dir"
		git init -q -b main
		git config user.email t@example.com
		git config user.name Test
		printf 'row one\n' >docs/STATUS.md
		printf 'plan one\n' >docs/plan/README.md
		printf 'echo one\n' >scripts/thing.sh
		git add -A && git commit -qm base

		git checkout -qb feature
		case "$kind" in
		status | both) printf 'row one changed by the branch\n' >docs/STATUS.md ;;
		esac
		case "$kind" in
		code | both) printf 'echo changed by the branch\n' >scripts/thing.sh ;;
		esac
		printf 'unrelated\n' >feature-only.txt
		git add -A && git commit -qm feature

		# origin/main advances with its own edit to the same lines.
		git checkout -q main
		case "$kind" in
		status | both) printf 'row one changed by main\n' >docs/STATUS.md ;;
		esac
		case "$kind" in
		code | both) printf 'echo changed by main\n' >scripts/thing.sh ;;
		esac
		printf 'main only\n' >main-only.txt
		git add -A && git commit -qm main-advance

		# A local "origin" so `git fetch origin main` resolves without a network.
		git remote add origin "$dir"
		git fetch -q origin main
		git checkout -q feature
	)
	printf '%s' "$dir"
}

# expect NAME WANT_RC DIR ARGS... — run the checker inside DIR.
expect() {
	local name="$1" want_rc="$2" dir="$3" got_rc=0
	shift 3
	LAST_OUT="$(cd "$dir" && "$CHECKER" --repo o/r --state-dir "$dir/state" "$@" 2>&1)" || got_rc=$?
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-26s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-26s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

assert_output() {
	if grep -qF -- "$2" <<<"$LAST_OUT"; then
		printf 'ok   %-26s output names %s\n' "$1" "$2"
	else
		printf 'FAIL %-26s output does not name %s\n%s\n' "$1" "$2" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

export GH_ENQUEUE_COUNT=1 GH_QUEUE_ENTRY="" GH_STATE=OPEN GH_DRAFT=false GH_BASE=main

# The healthy case first: without it every refusal below proves nothing.
STATUS_REPO="$(new_repo status-conflict status)"
expect eligible-status-only 0 "$STATUS_REPO" --assess 42
assert_output eligible-status-only 'docs/STATUS.md'

CLEAN_REPO="$(new_repo no-conflict none)"
expect eligible-no-conflict 0 "$CLEAN_REPO" --assess 42
assert_output eligible-no-conflict 'no conflicts at all'

# The policy guard: a conflict in code is the maintainer's to read, because the
# rebase changes what they approved. Both the code-only and the mixed case must
# refuse — the mixed one is the trap, since a driver-owned file is present too.
CODE_REPO="$(new_repo code-conflict code)"
expect refuse-code-conflict 1 "$CODE_REPO" --assess 42
assert_output refuse-code-conflict 'scripts/thing.sh'

BOTH_REPO="$(new_repo both-conflict both)"
expect refuse-mixed-conflict 1 "$BOTH_REPO" --assess 42
assert_output refuse-mixed-conflict 'scripts/thing.sh'

# Never a first enqueue: with no human enqueue on record there is nothing to
# restore, and enqueueing would be an agent deciding to merge.
GH_ENQUEUE_COUNT=0 expect refuse-never-enqueued 1 "$STATUS_REPO" --assess 42
assert_output refuse-never-enqueued 'no human has enqueued'

# Already queued: re-enqueueing would double-add.
GH_QUEUE_ENTRY='{"state":"QUEUED"}' expect refuse-already-queued 1 "$STATUS_REPO" --assess 42
assert_output refuse-already-queued 'already in the merge queue'

GH_STATE=MERGED expect refuse-not-open 1 "$STATUS_REPO" --assess 42
GH_DRAFT=true expect refuse-draft 1 "$STATUS_REPO" --assess 42

# --confirm fails closed. A session that lost its assessment must wake a human
# rather than fall back to enqueueing.
FRESH_REPO="$(new_repo confirm-fresh none)"
expect refuse-confirm-unassessed 1 "$FRESH_REPO" --confirm 42
assert_output refuse-confirm-unassessed 'no recorded assessment'

expect confirm-after-eligible 0 "$STATUS_REPO" --confirm 42

# A recorded refusal must not be readable as consent.
expect confirm-after-wake 1 "$CODE_REPO" --confirm 42
assert_output confirm-after-wake "was 'WAKE'"

# The base moving under the assessment invalidates it: the conflict set was
# measured against a different branch.
GH_BASE=release expect refuse-confirm-base-moved 1 "$STATUS_REPO" --confirm 42
assert_output refuse-confirm-base-moved 'base changed'

# A probe that cannot run must not read as "found no conflicts". Pointing the
# base at a ref that does not resolve is the cheapest way to break it, and the
# answer has to be exit 2 rather than a clean ELIGIBLE.
GH_BASE=nonexistent-branch expect refuse-unmeasurable 2 "$STATUS_REPO" --assess 42

# Usage errors are exit 2, so a malformed call cannot read as a refusal.
expect usage-no-mode 2 "$STATUS_REPO" 42
expect usage-bad-pr 2 "$STATUS_REPO" --assess not-a-number
expect usage-unknown-arg 2 "$STATUS_REPO" --assess 42 --bogus

# DRIVER_OWNED is what the whole policy turns on, and it is a hand-maintained
# list whose only guard was a comment saying to keep it in step with
# .gitattributes. Reconciled here in BOTH directions: a path that gains a
# `merge=` attribute and not an entry silently narrows the discount, and an
# entry with no attribute silently widens it — which is a session merging
# something the maintainer did not sign off on. Q799 added the third driver and
# this list did not notice; piped-gate's overlap_ignore caught its own half only
# because TestShippedRegistryCarriesRepoStateSettings does exactly this.
attributed="$(awk '
	/^[ \t]*#/ { next }
	/merge=/ { print $1 }
' "$REPO_ROOT/.gitattributes" | sort)"
listed="$(awk '
	/^DRIVER_OWNED=\(/ { inside = 1; next }
	inside && /^\)/ { exit }
	inside { gsub(/[ \t]/, ""); if ($0 != "") print }
' "$CHECKER" | sort)"
if [[ -z "$attributed" ]]; then
	printf 'FAIL %-26s .gitattributes lists no merge-driver-owned paths\n' driver-owned-source >&2
	fails=$((fails + 1))
elif [[ "$attributed" == "$listed" ]]; then
	printf 'ok   %-26s matches .gitattributes (%s)\n' driver-owned-reconciled "$(tr '\n' ' ' <<<"$listed")"
else
	printf 'FAIL %-26s DRIVER_OWNED and .gitattributes disagree\n  .gitattributes: %s\n  DRIVER_OWNED:   %s\n' \
		driver-owned-reconciled "$(tr '\n' ' ' <<<"$attributed")" "$(tr '\n' ' ' <<<"$listed")" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\n%d pr-requeue-eligible assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\npr-requeue-eligible-test: all assertions passed\n'
