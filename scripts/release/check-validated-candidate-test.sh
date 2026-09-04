#!/usr/bin/env bash
# check-validated-candidate-test.sh — asserts every direction of the publish-time
# validated-candidate gate.
#
# The rule it guards ran only in prose until Q879, which is a control that has
# never been tested against a case it should catch — so the case that filed the row
# is pinned here directly: a release line whose NEWEST candidate carries no
# validation, and an older one that does. A check keyed on `git tag --list` picks
# the newest and passes; this one has to name the older, validated candidate.
#
# The decision layer is what this script owns — which marker is the reference, does
# it agree with its tag, is it on this history, what does the delegate's verdict
# mean — and it runs against a repository this script builds, with the surface
# check stubbed. Deriving the released surface for real runs `go list -deps` over
# every module the Dockerfile builds; check-artifact-unchanged-test.sh owns that.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
SUBJECT="$SCRIPT_DIR/check-validated-candidate.sh"

pass=0
fail=0
ok() {
	printf '[check-validated-candidate-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-validated-candidate-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

WORK="$(mktemp -d)"
FIXTURE="$WORK/repo"
STUB="$WORK/surface-stub.sh"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# Stands in for check-artifact-unchanged.sh, exiting the code named in STUB_EXIT so
# each verdict the subject has to map can be posed directly — including the exit 2
# that means "could not measure", which must never read as a stop-ship.
cat >"$STUB" <<'STUB_BODY'
#!/usr/bin/env bash
set -euo pipefail
case "${STUB_EXIT:-0}" in
0) echo "check-artifact-unchanged: ok (stub, none on the released surface)"; exit 0 ;;
1)
	echo "check-artifact-unchanged: 1 file(s) on the released surface changed:" >&2
	echo "  charts/actions-gateway/README.md" >&2
	exit 1
	;;
*)
	echo "check-artifact-unchanged: semverfloor -ships failed (stub)" >&2
	exit "${STUB_EXIT}"
	;;
esac
STUB_BODY
chmod +x "$STUB"

# git in CI has no committer identity and may have signing configured globally.
fixture_git() {
	git -C "$FIXTURE" \
		-c user.name='fixture' -c user.email='fixture@example.invalid' \
		-c commit.gpgsign=false -c tag.gpgsign=false "$@"
}

commit_file() {
	printf '%s\n' "$2" >"$FIXTURE/$1"
	fixture_git add "$1"
	fixture_git commit -q -m "$1: $2"
	fixture_git rev-parse HEAD
}

LAST_OUT=""
# run_case DESC WANT_EXIT STUB_EXIT [ARGS...] — invoke the subject in the fixture.
run_case() {
	local desc="$1" want="$2" stub_exit="$3"
	shift 3
	local out rc=0
	out="$(cd "$FIXTURE" &&
		STUB_EXIT="$stub_exit" CHECK_VALIDATED_SURFACE_CHECK="$STUB" \
			"$SUBJECT" "$@" 2>&1)" || rc=$?
		die_if_killed "$desc" "$rc" "$want"
	LAST_OUT="$out"
	if [[ "$rc" == "$want" ]]; then
		ok "$desc (exit ${rc})"
	else
		bad "$desc: want exit ${want}, got ${rc}"
		printf '%s\n' "$out" >&2
	fi
}

want_out() {
	if [[ "$LAST_OUT" == *"$2"* ]]; then
		ok "$1"
	else
		bad "$1: output does not contain '$2'"
		printf '%s\n' "$LAST_OUT" >&2
	fi
}

want_not_out() {
	if [[ "$LAST_OUT" != *"$2"* ]]; then
		ok "$1"
	else
		bad "$1: output unexpectedly contains '$2'"
		printf '%s\n' "$LAST_OUT" >&2
	fi
}

mkdir -p "$FIXTURE"
git -c init.defaultBranch=main init -q "$FIXTURE"
# A fixture repo must not run background git maintenance: it outlives the suite
# and races the traces the gate reads. docs/development/testing.md#a-fixture-repo-must-not-run-background-git
fixture_git config maintenance.auto false

# A release line whose history is: rc.1 (validated), rc.2 (NOT validated, and the
# newest candidate tag), then the stable tag. This is the v1.5.0 shape.
rc1_sha="$(commit_file a.txt one)"
fixture_git tag -a v1.5.0-rc.1 -m 'rc.1'
rc2_sha="$(commit_file b.txt two)"
fixture_git tag -a v1.5.0-rc.2 -m 'rc.2'
commit_file c.txt three >/dev/null
fixture_git tag -a v1.5.0 -m 'stable'

run_case "usage with no argument" 2 0
run_case "an unknown tag cannot be measured" 2 0 v9.9.9
want_out "the unknown tag is named" "not a tag or commit: v9.9.9"

run_case "a prerelease is the artifact validated, not a subject of this check" 0 0 v1.5.0-rc.2
want_out "the prerelease exemption says why" "it is the artifact that gets validated"

run_case "a stable tag with no marker anywhere is a stop-ship" 1 0 v1.5.0
want_out "the failure names the namespace to look in" "refs/validated/"
want_out "the failure routes to the runbook" "validate-the-release-candidate-on-dogfood"
# The message has to carry the recorder command rather than pointing at one the
# gate prints only on its own failure path: a maintainer whose record never ran
# has never seen it.
want_out "the failure prints the recorder command" "record-validated-candidate.sh v1.5.0-rc.N"

# THE ROW'S CASE. rc.2 is the newest candidate and validated nothing; rc.1 is the
# reference. A check reading `git tag --list` picks rc.2 and passes on a candidate
# that never ran the gate, which is how v1.5.0-rc.2 reached publish.
fixture_git update-ref refs/validated/v1.5.0-rc.1 "$rc1_sha"
run_case "the newest VALIDATED candidate is the reference, not the newest tag" 0 0 v1.5.0
want_out "the passing line names rc.1" "v1.5.0-rc.1 validated"
want_not_out "the unvalidated newer candidate is not the reference" "v1.5.0-rc.2 validated"

# ...and once rc.2 is validated too, it supersedes rc.1.
fixture_git update-ref refs/validated/v1.5.0-rc.2 "$rc2_sha"
run_case "a newer validated candidate supersedes an older one" 0 0 v1.5.0
want_out "the passing line names rc.2" "v1.5.0-rc.2 validated"

run_case "the released surface moving after validation is a stop-ship" 1 1 v1.5.0
want_out "the finding lists the file the delegate named" "charts/actions-gateway/README.md"
want_out "the finding names the candidate it invalidates" "v1.5.0-rc.2 is the newest validated candidate"

run_case "a delegate that could not measure is exit 2, never a stop-ship" 2 2 v1.5.0
want_out "the unmeasurable case says which delegate failed" "surface check failed"

# A marker whose sha disagrees with the tag it names: the gate ran against one
# commit and the tag names another, which is the stale-tag incident from the other
# side. Repointed rather than re-created — a ref update is not a compare-and-swap
# locally, and posing the state is the point.
fixture_git update-ref refs/validated/v1.5.0-rc.2 "$rc1_sha"
run_case "a marker that disagrees with its own tag is a stop-ship" 1 0 v1.5.0
want_out "the disagreement prints both commits" "does not point at the commit it was validated at"
fixture_git update-ref refs/validated/v1.5.0-rc.2 "$rc2_sha"

# A verdict from a different line of history. Built on an orphan branch so the
# validated commit shares no ancestry with the tag at all.
fixture_git checkout -q --orphan sidetrack
fixture_git rm -q -rf .
side_sha="$(commit_file d.txt elsewhere)"
fixture_git checkout -q main
fixture_git update-ref refs/validated/v1.5.0-rc.3 "$side_sha"
fixture_git tag -a v1.5.0-rc.3 "$side_sha" -m 'rc.3 off-history'
run_case "a validation from another line of history is a stop-ship" 1 0 v1.5.0
want_out "the off-history failure says so" "not an ancestor"
fixture_git tag -d v1.5.0-rc.3 >/dev/null
fixture_git update-ref -d refs/validated/v1.5.0-rc.3

# A marker naming a tag this checkout does not have cannot be cross-checked, and
# refusing to measure is the answer — publish.yml checks out at full depth, so an
# absent tag there is an anomaly rather than a shallow clone.
fixture_git update-ref refs/validated/v1.5.0-rc.9 "$rc2_sha"
run_case "a marker whose tag is absent cannot be measured" 2 0 v1.5.0
want_out "the unmeasurable case names the missing tag" "v1.5.0-rc.9"
# Not only the shallow-clone diagnosis: a deleted tag leaves the marker as the
# stale half, and clearing it is the only repair anything names.
want_out "the unmeasurable case says how to clear a stale marker" "git push origin --delete refs/validated/v1.5.0-rc.9"
fixture_git update-ref -d refs/validated/v1.5.0-rc.9

# Markers are per release line: 1.4's validation says nothing about 1.5.
fixture_git tag -a v1.4.0 -m '1.4' "$rc1_sha"
fixture_git tag -a v1.4.0-rc.1 -m '1.4 rc.1' "$rc1_sha"
fixture_git update-ref refs/validated/v1.4.0-rc.1 "$rc1_sha"
fixture_git update-ref -d refs/validated/v1.5.0-rc.1
fixture_git update-ref -d refs/validated/v1.5.0-rc.2
run_case "a marker from another release line does not cover this tag" 1 0 v1.5.0
want_out "the cross-line case reports no validation for this line" "no candidate for v1.5.0 has a recorded validation"

# A PATCH tag gets no exemption. The documented patch procedure used to cut no
# candidate at all, and announce-bar, the sibling stop-ship gate, does exempt a
# backport, so "patches are exempt" is the reading to expect. The decision
# recorded in docs/operations/release.md is that every stable tag needs a
# validated candidate of its own line, and this is what holds it.
patch_sha="$(fixture_git rev-parse HEAD)"
fixture_git tag -a v1.5.1 -m 'patch' "$patch_sha"
run_case "a patch tag with no candidate of its own is a stop-ship" 1 0 v1.5.1
want_out "the patch case is named, because it is the surprising one" "A patch line needs its own candidate"

fixture_git tag -a v1.5.1-rc.1 -m 'patch rc' "$patch_sha"
fixture_git update-ref refs/validated/v1.5.1-rc.1 "$patch_sha"
run_case "a patch tag with its own validated candidate publishes" 0 0 v1.5.1
want_out "the passing line names the patch candidate" "v1.5.1-rc.1 validated"

# ...and the minor's marker does not stand in for the patch's.
fixture_git update-ref -d refs/validated/v1.5.1-rc.1
fixture_git update-ref refs/validated/v1.5.0-rc.2 "$rc2_sha"
run_case "the minor's validation does not cover its patch" 1 0 v1.5.1
want_out "the cross-line refusal names the patch tag" "no candidate for v1.5.1 has a recorded validation"

printf '[check-validated-candidate-test] %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
