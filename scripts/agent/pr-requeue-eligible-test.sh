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
# the real invocation prints after gh's own --jq has run, including the node id
# the queue query selects so that a read which happened is distinguishable from
# one that did not.
#
# Two failure knobs, because the checker has to tell them apart from a measured
# answer and from each other: GH_FAIL names a read that exits non-zero with
# nothing on stdout (a transport failure), GH_SILENT one that exits 0 with
# nothing on stdout (the 2026-08-11 shape, an empty read reported as success).
cat >"$FIXTURE_DIR/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
read_kind=""
case "$1 ${2-}" in
"pr view") read_kind=view ;;
"api graphql") read_kind=graphql ;;
"api "*) read_kind=timeline ;;
*)
	printf 'gh stub: unhandled: %s\n' "$*" >&2
	exit 1
	;;
esac
if [[ "${GH_FAIL:-}" == "$read_kind" ]]; then
	printf 'gh stub: simulated transport failure\n' >&2
	exit 1
fi
if [[ "${GH_SILENT:-}" == "$read_kind" ]]; then
	exit 0
fi
case "$read_kind" in
view) printf '%s\t%s\t%s\n' "${GH_STATE:-OPEN}" "${GH_DRAFT:-false}" "${GH_BASE:-main}" ;;
graphql) printf '%s %s\n' "${GH_NODE_ID:-PR_kwTEST}" "${GH_QUEUE_ENTRY:-none}" ;;
timeline) printf '%s\n' "${GH_ENQUEUE_COUNT:-0}" ;;
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

assert_eq() {
	if [[ "$2" == "$3" ]]; then
		printf 'ok   %-26s %s\n' "$1" "$2"
	else
		printf 'FAIL %-26s want %s got %s\n' "$1" "$3" "$2" >&2
		fails=$((fails + 1))
	fi
}

# field DIR KEY — the last value recorded for KEY in PR 42's verdict file. Last
# rather than first: the file accumulates one record per assessment.
field() {
	awk -v k="$2" '$1 == k { v = $2 } END { print v }' "$1/state/42.verdict"
}

export GH_ENQUEUE_COUNT=1 GH_QUEUE_ENTRY=none GH_STATE=OPEN GH_DRAFT=false GH_BASE=main
export GH_FAIL="" GH_SILENT="" GH_NODE_ID=PR_kwTEST

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
# restore, and enqueueing would be an agent deciding to merge. On its own repo,
# because a refusal is recorded now and would otherwise land on the ELIGIBLE
# record the confirm cases below read.
NEVER_REPO="$(new_repo never-enqueued status)"
GH_ENQUEUE_COUNT=0 expect refuse-never-enqueued 1 "$NEVER_REPO" --assess 42
assert_output refuse-never-enqueued 'no human has enqueued'

# A refusal taken before the probe still leaves a record, so a later reader can
# tell "assessed and refused" from "never assessed" — and the OIDs it could not
# measure read as `-` rather than as a stale pair.
assert_eq refuse-records-verdict "$(field "$NEVER_REPO" verdict)" WAKE
assert_eq refuse-records-no-oid "$(field "$NEVER_REPO" base_oid)" -

# Already queued: re-enqueueing would double-add.
QUEUED_REPO="$(new_repo already-queued status)"
GH_QUEUE_ENTRY=QUEUED expect refuse-already-queued 1 "$QUEUED_REPO" --assess 42
assert_output refuse-already-queued 'already in the merge queue'

CLOSED_REPO="$(new_repo not-open status)"
GH_STATE=MERGED expect refuse-not-open 1 "$CLOSED_REPO" --assess 42
DRAFT_REPO="$(new_repo draft status)"
GH_DRAFT=true expect refuse-draft 1 "$DRAFT_REPO" --assess 42

# A read that did not happen is not a measured answer (Q805). Each of these
# three reads leaves an empty string behind, and every check downstream of one
# reads emptiness as a finding: no state as "not OPEN", no queue entry as "not
# queued", no timeline as "nobody enqueued it". Exit 2, not 1: a WAKE records a
# reason, and a reason no read supports is the history the rebase then makes
# unfalsifiable. Both shapes are covered per read, since a transport failure and
# an empty success arrive as different statuses and the same output.
#
# On their own repo, because an unmeasurable assessment is recorded like a
# refusal and would otherwise land on the ELIGIBLE record the confirm cases
# below read.
UNREADABLE_REPO="$(new_repo unreadable status)"
for failing in view graphql timeline; do
	GH_FAIL="$failing" expect "unmeasurable-fail-$failing" 2 "$UNREADABLE_REPO" --assess 42
	assert_output "unmeasurable-fail-$failing" 'refusing to guess'
	GH_SILENT="$failing" expect "unmeasurable-empty-$failing" 2 "$UNREADABLE_REPO" --assess 42
	assert_output "unmeasurable-empty-$failing" 'refusing to guess'
done

# Recorded, so that a later reader can tell a read that could not be taken from
# an assessment that never ran (Q810). The first read is the one that fails
# before the base is even known, so the record carries `-` for it rather than
# failing on an unset variable.
assert_eq unmeasurable-records-verdict "$(field "$UNREADABLE_REPO" verdict)" UNMEASURABLE
GH_FAIL=view expect unmeasurable-before-base 2 "$UNREADABLE_REPO" --assess 42
assert_eq unmeasurable-records-no-base "$(field "$UNREADABLE_REPO" base)" -

# gh's --jq prints nothing for a JSON null, so "not queued" and "never read"
# are the same empty answer at that layer and the query selects the PR's node id
# to tell them apart. An answer without one is a read that did not land, however
# gh exited.
GH_NODE_ID=- expect unmeasurable-queue-no-id 2 "$UNREADABLE_REPO" --assess 42
assert_output unmeasurable-queue-no-id 'refusing to guess'

# `--confirm` re-reads both live probes before enqueueing, so it needs the same
# refusal: this is the path that runs `gh pr merge` on a 0.
GH_FAIL=graphql expect unmeasurable-confirm 2 "$STATUS_REPO" --confirm 42
assert_output unmeasurable-confirm 'refusing to guess'

# `gh api --paginate` runs its --jq per page and prints one count per page, so
# past 100 timeline events the count arrives multi-line. Read as one number that
# is an arithmetic syntax error, and it surfaced as "no human enqueued this PR"
# with nothing wrong at all. The pages are summed: a human enqueue on page two
# still counts, and pages that are all zero still refuse.
PAGED_REPO="$(new_repo paged-timeline status)"
GH_ENQUEUE_COUNT=$'0\n1\n0' expect paged-timeline-sums 0 "$PAGED_REPO" --assess 42
PAGED_ZERO_REPO="$(new_repo paged-timeline-zero status)"
GH_ENQUEUE_COUNT=$'0\n0\n0' expect paged-timeline-zero 1 "$PAGED_ZERO_REPO" --assess 42
assert_output paged-timeline-zero 'no human has enqueued'

# A non-numeric answer is not a count. gh prints its errors on stderr, but a
# body that parses as text rather than a number has to refuse rather than be
# coerced to 0 by the arithmetic.
GH_ENQUEUE_COUNT='unexpected end of JSON input' \
	expect unmeasurable-timeline-garbage 2 "$UNREADABLE_REPO" --assess 42
assert_output unmeasurable-timeline-garbage 'refusing to guess'

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

# The records accumulate and the LAST one governs (Q810). Overwriting instead
# would let a refusal that never probed erase the measurement the eviction rests
# on; reading anything but the last would let a stale ELIGIBLE authorise an
# enqueue the current state refuses.
LAST_REPO="$(new_repo last-record-wins status)"
expect last-record-first-assess 0 "$LAST_REPO" --assess 42
GH_QUEUE_ENTRY='{"state":"QUEUED"}' expect last-record-second-assess 1 "$LAST_REPO" --assess 42
assert_eq last-record-kept-both \
	"$(grep -c '^verdict ' "$LAST_REPO/state/42.verdict")" 2
expect last-record-confirm-refuses 1 "$LAST_REPO" --confirm 42
assert_output last-record-confirm-refuses "was 'WAKE'"

# The point of the whole record (Q810): once the branch heals, the conflict is
# unreconstructable from the refs — but the two OIDs the assessment recorded
# still re-derive it. Without this the eviction's cause dies with the rebase,
# and a dispatcher's later read cannot be reconciled with the worker's.
HEAL_REPO="$(new_repo heal-erases-conflict status)"
expect heal-assess 0 "$HEAL_REPO" --assess 42
assert_output heal-assess 'measured: git merge-tree --write-tree'
heal_base_oid="$(field "$HEAL_REPO" base_oid)"
heal_head_oid="$(field "$HEAL_REPO" head_oid)"
assert_eq heal-records-conflict "$(field "$HEAL_REPO" conflict)" docs/STATUS.md

(
	cd "$HEAL_REPO"
	printf 'row one reconciled by the rebase\n' >docs/STATUS.md
	git add docs/STATUS.md
	git commit -qm heal
	git rebase -q origin/main >/dev/null 2>&1 || {
		printf 'row one reconciled by the rebase\n' >docs/STATUS.md
		git add docs/STATUS.md
		GIT_EDITOR=true git rebase --continue >/dev/null 2>&1
	}
)

post_heal="$(cd "$HEAL_REPO" && git merge-tree --write-tree origin/main HEAD | grep -c '^CONFLICT' || true)"
assert_eq heal-hides-conflict "$post_heal" 0

# merge-tree exits 1 on a conflict, which is the expected answer here, so the
# pipeline's status is not the assertion — its output is.
replayed="$(cd "$HEAL_REPO" &&
	git merge-tree --write-tree "$heal_base_oid" "$heal_head_oid" |
	awk '/^CONFLICT/ && match($0, / in .+$/) { print substr($0, RSTART + 4) }')" || true
assert_eq heal-replay-reconstructs "$replayed" docs/STATUS.md

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
