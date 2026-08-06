#!/usr/bin/env bash
#
# Unit tests for scripts/release/release-delta.sh — the unreleased-delta report.
#
# The report's load-bearing claim is that it needs no bookkeeping: everything it
# prints is derived from commit subjects and from STATUS.md's history. These
# fixtures pin the derivations that are not obvious — a Queue row PARKED in
# Deferred is not delivered work, a row resurrected by a bad merge resolution is
# counted once, and an empty API path list must print "(none)" rather than
# widening the diff to the whole repo. Runs under `make check` (via
# `make scripts-test`).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/release/release-delta.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# status QUEUE_IDS DEFERRED_IDS — write docs/STATUS.md with the given
# space-separated IDs in each table, in the section shape the script parses.
status() {
	local queue="$1" deferred="$2" id
	mkdir -p docs
	{
		printf '# Project Status\n\n## Progress\n\n'
		printf '| Item | Labels | Status |\n|---|---|---|\n'
		printf '| <a id="Q999"></a>[A plan](plan/x.md) | infra | ✅ |\n'
		printf '\n## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
		for id in $queue; do
			printf '| <a id="%s"></a>%s | Thing | infra | 🔲 | S | note |\n' "$id" "$id"
		done
		printf '\n## Deferred\n\n'
		printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
		for id in $deferred; do
			printf '| <a id="%s"></a>%s | Thing | infra | S | **Demand:** someone asks. |\n' "$id" "$id"
		done
	} >docs/STATUS.md
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
		git config user.email t@t.t
		git config user.name t

		status "Q1 Q2 Q3 Q4 Q5" ""
		printf 'seed\n' >README.md
		commit "chore: seed"
		git tag v1.0.0

		printf 'more\n' >>README.md
		commit "docs: narrate something"
		git rev-parse HEAD >"$WORKDIR/c_docs"

		# Delivered: the row leaves the Queue and goes nowhere else.
		status "Q2 Q3 Q4 Q5" ""
		commit "fix(agc): fix a thing (Q1)"
		git rev-parse HEAD >"$WORKDIR/c_fix"

		# Parked, not delivered: Queue -> Deferred in one commit.
		status "Q3 Q4 Q5" "Q2"
		commit "feat(gmc): add a thing, park Q2"

		mkdir -p cmd/agc/api/v1alpha1
		printf 'package v1alpha1\n' >cmd/agc/api/v1alpha1/types.go
		commit "refactor(api)!: rename a published field"

		status "Q4 Q5" "Q2"
		commit "chore: drop Q3"

		# A row main deleted comes back through a bad merge resolution, then is
		# dropped again: one delivery, not two.
		status "Q3 Q4 Q5" "Q2"
		commit "chore: resurrect Q3"
		status "Q4 Q5" "Q2"
		commit "chore: drop Q3 again"

		# Q5 leaves and is re-filed, and is open at TO: not delivered work.
		mkdir -p docs/operations
		printf 'upgrade\n' >docs/operations/upgrade.md
		status "Q4" "Q2"
		commit "perf(proxy): speed up the tunnel" "BREAKING CHANGE: a values key was renamed."

		printf 'x\n' >>README.md
		status "Q4 Q5" "Q2"
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
		git config user.email t@t.t
		git config user.name t
		status "Q1" ""
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
want 'commit count excludes FROM' "$out" '^9 commits'

want 'type histogram: feat' "$out" '^ +1 +feat$'
want 'type histogram: fix' "$out" '^ +1 +fix$'
want 'type histogram: chore' "$out" '^ +3 +chore$'
want 'type histogram: non-conventional' "$out" '^ +1 +\(non-conventional\)$'

want 'breaking: ! subject' "$out" 'refactor\(api\)!: rename a published field'
want 'breaking: BREAKING CHANGE body' "$out" 'perf\(proxy\): speed up the tunnel'

want 'closed row names its commit' "$out" '^ +Q1 +fix\(agc\): fix a thing \(Q1\)$'
want_no 'parked row is not closed' "$out" '^ +Q2 '
want_no 'row still in the Queue is not closed' "$out" '^ +Q4 '
want_no 'row re-filed and open at TO is not closed' "$out" '^ +Q5 '
want 'resurrected row keeps its first removal' "$out" '^ +Q3 +chore: drop Q3$'
want_no 'resurrected row is not listed twice' "$out" 'chore: drop Q3 again'
want_no 'Progress-table anchor is not an item' "$out" 'Q999'

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

if ((fails)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nrelease-delta: all assertions passed\n'
