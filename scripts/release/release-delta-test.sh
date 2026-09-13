#!/usr/bin/env bash
#
# Unit tests for scripts/release/release-delta.sh — the unreleased-delta report.
#
# The report's load-bearing claim is that it needs no bookkeeping: everything it
# prints is derived from commit subjects and from the item store's history.
# These fixtures pin the derivations that are not obvious — a PARKED row is a
# `status:` edit and so is never a deletion at all, a flake-watch retirement is
# delivered work from an EARLIER release and must not be credited here, a row
# resurrected by a bad merge resolution is counted once, a closure beyond HEAD
# prints its verb as `-` rather than guessing, and an empty API path list must
# print "(none)" rather than widening the diff to the whole repo. Runs under
# `make check` (via `make scripts-test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/release/release-delta.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# row ID [STATUS] — write one item file in the store's shape. Only the filename
# is load-bearing for this report (the walk reads paths, and queue.py reads the
# deleting commit's message), but a realistic body keeps the fixture honest.
row() {
	local id="$1" status="${2:-ready}"
	mkdir -p docs/queue
	cat >"docs/queue/$id.md" <<EOF
---
id: $id
rank: b${id#Q}
labels:
    - debt
status: $status
---

# $id — a thing that needs doing

Notes.
EOF
}

# ledger_seed — the flake-watch ledger a retirement writes into.
ledger_seed() {
	mkdir -p docs/development
	printf '# Retired flakes\n\n| ID | Symptom | Fix | Retired | Bar |\n|---|---|---|---|---|\n' \
		>docs/development/flake-watch-retired.md
}

# ledger_add ID — record ID as retired, the line a retiring commit appends.
ledger_add() {
	printf '| %s | flaky thing | #1 | 2026-01-01 | Soaked |\n' "$1" \
		>>docs/development/flake-watch-retired.md
}

# ledger_refuted ID — a REFUTED ledger row: first cell `none`, the id named only
# in the narrative. The shape that separates a first-cell anchor from a bare-id
# one, taken from 3e1770a327 on main.
ledger_refuted() {
	printf '| none | flaky thing | none | 2026-01-01 | Refuted: filed as %s, never observed |\n' \
		"$1" >>docs/development/flake-watch-retired.md
}

# commit SUBJECT [BODY] — commit whatever is staged plus the STATUS.md state.
commit() {
	local subject="$1" body="${2:-}"
	git add -A
	if [[ -n "$body" ]]; then
		git commit -q -m "$subject" -m "$body"
	else
		git commit -q -m "$subject"
	fi
}

# A history exercising every derivation the report makes. Echoes the repo path;
# SHAs of the two intermediate cut points land in C_DOCS and C_FIX.
build_repo() {
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

		row Q1
		row Q2
		row Q3
		row Q4
		row Q5
		row Q6
		row Q7
		# Not an item: the store holds prose beside its rows, and a path whose
		# stem is not an id must never be read as a closure.
		printf 'the store\n' >docs/queue/README.md
		ledger_seed
		printf 'seed\n' >README.md
		commit "chore: seed"
		git tag v1.0.0

		printf 'more\n' >>README.md
		commit "docs: narrate something"
		git rev-parse HEAD >"$WORKDIR/c_docs"

		# Delivered: the row's file is deleted, and the row commit says how.
		git rm -q docs/queue/Q1.md
		commit "fix(agc): fix a thing (Q1)" "docs(queue): complete Q1"
		git rev-parse HEAD >"$WORKDIR/c_fix"

		# Parked, not delivered: a `status:` edit, so the file survives and the
		# walk never sees a deletion. No Deferred subtraction is needed for it.
		row Q2 deferred
		commit "feat(gmc): add a thing, park Q2"

		mkdir -p cmd/agc/api/v1alpha1
		printf 'package v1alpha1\n' >cmd/agc/api/v1alpha1/types.go
		commit "refactor(api)!: rename a published field"

		git rm -q docs/queue/Q3.md
		commit "chore: drop Q3" "docs(queue): prune Q3"

		# A row main deleted comes back through a bad merge resolution, then is
		# dropped again: one delivery, not two.
		row Q3
		commit "chore: resurrect Q3"
		git rm -q docs/queue/Q3.md
		commit "chore: drop Q3 again" "docs(queue): prune Q3 again"

		# A soaked flake leaves for the ledger. Its delivery was the earlier fix
		# PR, which only parked it, so this window must not be credited with it.
		# Q7 is delivered in the SAME commit, and a refuted ledger row names it in
		# its narrative while retiring nothing. A bare-id line filter would read
		# that mention as Q7 being retired and silently drop a delivered row;
		# only the first cell says what a ledger line retires.
		git rm -q docs/queue/Q6.md docs/queue/Q7.md
		ledger_add Q6
		ledger_refuted Q7
		commit "docs(queue): retire Q6, soaked" "docs(queue): close Q7"

		git rm -q docs/queue/README.md
		commit "chore: drop the store's own README"

		# Q5 leaves and is re-filed, and is present at TO: not delivered work.
		mkdir -p docs/operations
		printf 'upgrade\n' >docs/operations/upgrade.md
		git rm -q docs/queue/Q5.md
		commit "perf(proxy): speed up the tunnel" "BREAKING CHANGE: a values key was renamed."

		printf 'x\n' >>README.md
		row Q5
		commit "WIP nonsense"

		# Newer than v1.0.0 but not a release: the default FROM must skip it.
		git tag v1.1.0-rc.1
	)
	printf '%s\n' "$d"
}

# A repo with neither an API tree nor docs/operations, so both diffstats run
# with an empty pathspec list. Echoes the repo path.
build_pathless_repo() {
	local d="$WORKDIR/pathless"
	rm -rf "$d"
	mkdir -p "$d"
	(
		cd "$d"
		git init -q -b main
		git config maintenance.auto false
		git config user.email t@t.t
		git config user.name t
		row Q1
		printf 'seed\n' >README.md
		commit "chore: seed"
		git tag v1.0.0
		printf 'more\n' >>README.md
		commit "fix(x): change something outside every watched tree"
	)
	printf '%s\n' "$d"
}

# want NAME OUTPUT PATTERN — assert the report matched an extended regexp.
want() {
	local name="$1" out="$2" pattern="$3"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: no match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	fi
}

# section_of OUTPUT TITLE_PREFIX — the body lines of one "== TITLE" section, so
# an assertion can be scoped to the section that owns it.
section_of() {
	awk -v t="== $2" 'index($0, t) == 1 { on = 1; next } /^== / { on = 0 } on' <<<"$1"
}

# want_no NAME OUTPUT PATTERN — assert the report did NOT match.
want_no() {
	local name="$1" out="$2" pattern="$3"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'FAIL %s: unexpected match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	else
		printf 'ok   %s\n' "$name"
	fi
}

repo="$(build_repo)"
c_docs="$(cat "$WORKDIR/c_docs")"
c_fix="$(cat "$WORKDIR/c_fix")"

out="$(cd "$repo" && "$SCRIPT")"

want 'default FROM skips RC tags' "$out" '^Release delta v1\.0\.0\.\.HEAD$'
want 'commit count excludes FROM' "$out" '^11 commits'

want 'type histogram: feat' "$out" '^ +1 +feat$'
want 'type histogram: fix' "$out" '^ +1 +fix$'
want 'type histogram: docs' "$out" '^ +2 +docs$'
want 'type histogram: chore' "$out" '^ +4 +chore$'
want 'type histogram: non-conventional' "$out" '^ +1 +\(non-conventional\)$'

want 'breaking: ! subject' "$out" 'refactor\(api\)!: rename a published field'
want 'breaking: BREAKING CHANGE body' "$out" 'perf\(proxy\): speed up the tunnel'

closed_section="$(section_of "$out" 'Queue rows closed')"
want 'closed row names its commit' "$closed_section" \
	'^ +Q1 +complete +fix\(agc\): fix a thing \(Q1\)$'
want_no 'parked row is not a deletion at all' "$closed_section" '^ +Q2 '
want_no 'row still in the store is not closed' "$closed_section" '^ +Q4 '
want_no 'row re-filed and present at TO is not closed' "$closed_section" '^ +Q5 '
want 'resurrected row keeps its first removal' "$closed_section" \
	'^ +Q3 +prune +chore: drop Q3$'
want_no 'resurrected row is not listed twice' "$closed_section" 'chore: drop Q3 again'
# The delivery moment for a flake was the fix PR that parked it, an earlier
# release. Crediting the retirement here bills this release for that work.
# Suppressed as a closure, but still a commit in the window: the `docs` count of
# 2 above is the retiring commit plus the narrating one.
want_no 'flake retired to the ledger is not closed here' "$closed_section" '^ +Q6 '
want 'an id named only in a ledger narrative is not retired by it' "$closed_section" \
	'^ +Q7 +close +'
want_no 'a non-item path in the store is not a closure' "$closed_section" 'README'
want_no 'every verb was read, so nothing prints as unknown' "$closed_section" '^ +Q[0-9]+ +- '

api_section="$(section_of "$out" 'API surface')"
want 'API diffstat lists the API tree' "$api_section" 'cmd/agc/api/v1alpha1/types\.go'
want_no 'API diffstat excludes non-API files' "$api_section" 'README'
want 'operator docs diffstat' "$(section_of "$out" 'Operator-facing docs')" 'docs/operations/upgrade\.md'

want 'commit-type counts are reported' "$out" '^Commit-type counts: 1 feat, 1 fix, 1 perf\.$'
want 'counts point at the floor for what ships' "$out" 'scripts/release/semver-floor\.sh v1\.0\.0$'
want 'breaking commits are flagged for judgement' "$out" '^ +2 breaking-marked commit'

# A window with no api/ tree at either end must print "(none)", not the whole
# repo's diffstat — an empty pathspec list would widen `git diff` to everything.
out="$(cd "$repo" && "$SCRIPT" v1.0.0 "$c_docs")"
api_section="$(section_of "$out" 'API surface')"
want 'empty API window prints none' "$api_section" '^ +\(none\)$'
want_no 'empty API window does not widen the diff' "$api_section" 'README\.md'
want 'docs-only window reports no typed commits' "$out" '^Commit-type counts: no feat/fix/perf commits'

out="$(cd "$repo" && "$SCRIPT" v1.0.0 "$c_fix")"
want 'fix-only window counts the fix' "$out" '^Commit-type counts: 0 feat, 1 fix, 0 perf\.$'

# An arbitrary (non-tag) FROM still counts, and still points at the floor.
out="$(cd "$repo" && "$SCRIPT" "$c_docs" "$c_fix")"
want 'non-tag FROM still counts commits' "$out" '^Commit-type counts: 0 feat, 1 fix, 0 perf\.$'

# With no watched tree present at all, both diffstats run on an empty pathspec
# list — which `git diff` would read as "everything".
pathless="$(build_pathless_repo)"
out="$(cd "$pathless" && "$SCRIPT")"
want 'no API tree present prints none' "$(section_of "$out" 'API surface')" '^ +\(none\)$'
want 'no operator docs tree prints none' "$(section_of "$out" 'Operator-facing docs')" '^ +\(none\)$'
want_no 'absent trees do not widen the diff' "$out" 'README\.md'

# `queue.py metrics --events` replays from HEAD, so a row closed between HEAD
# and TO has no verb to read. It must print `-` and say so, rather than being
# dropped or shown as an unclassified removal — the silent zero this report
# carried for a release cycle is exactly the failure mode being guarded here.
out="$(cd "$repo" && git -c advice.detachedHead=false checkout -q "$c_fix" &&
	"$SCRIPT" v1.0.0 main; rc=$?; git -C "$repo" checkout -q main; exit $rc)"
closed_section="$(section_of "$out" 'Queue rows closed')"
want 'a closure beyond HEAD still lists its row' "$closed_section" '^ +Q3 +'
want 'a closure beyond HEAD prints its verb as unknown' "$closed_section" '^ +Q3 +- +chore: drop Q3$'
want 'unread verbs are counted, not silently dropped' "$closed_section" \
	'row\(s\) above show - for the verb: closed beyond HEAD'
want 'a verb readable at HEAD is still read' "$closed_section" \
	'^ +Q1 +complete +'

# A verb replay that could not run AT ALL must not be reported as "closed beyond
# HEAD": that is a missing input rendered as a plausible answer, which is the
# defect class this whole section exists to fix. The report is not a gate, so it
# still exits 0 and still prints every other section.
cp "$SCRIPT" "$WORKDIR/orphan-release-delta.sh"
out="$(cd "$repo" && bash "$WORKDIR/orphan-release-delta.sh" 2>&1)"; rc=$?
closed_section="$(section_of "$out" 'Queue rows closed')"
want 'an unrunnable verb replay still reports its rows' "$closed_section" '^ +Q1 +- +'
want 'an unrunnable verb replay says so, not "beyond HEAD"' "$closed_section" \
	'every verb above reads -: queue.py metrics could not be run'
want_no 'an unrunnable replay is not blamed on HEAD' "$closed_section" 'closed beyond HEAD'
if ((rc == 0)); then
	printf 'ok   %s\n' 'a missing verb replay does not turn the report into a gate'
else
	printf 'FAIL %s: exited %d\n%s\n' 'a missing verb replay does not turn the report into a gate' "$rc" "$out" >&2
	fails=$((fails + 1))
fi

if ((fails)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nrelease-delta: all assertions passed\n'
