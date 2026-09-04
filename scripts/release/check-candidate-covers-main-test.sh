#!/usr/bin/env bash
# check-candidate-covers-main-test.sh — asserts every direction of the freeze watch.
#
# The rule this guards was, until Q1001, only ever evaluated by hand after the
# fact, which is a control never tested against a case it should catch. So the
# incident that filed the row is pinned here as a case: a candidate outstanding, a
# dependency bump landing on top of it, and the watch having to say so.
#
# Two fixtures, because the subject has two halves and they are testable at
# different prices.
#
#   * The decision layer — is a candidate outstanding, which prerelease is the
#     reference, what does the delegate's verdict mean — runs against a repository
#     this script builds, with the surface check stubbed. That is the half this
#     script owns, and it runs at any checkout depth.
#   * The released surface itself belongs to check-artifact-unchanged-test.sh.
#     Deriving it for real runs `go list -deps` over every module the Dockerfile
#     builds, which a synthetic fixture cannot pose without shipping a Go module
#     per image.
#
# The incident case at the end uses real history and real surface derivation, so
# it says it was skipped rather than passing for the wrong reason when the clone
# has no tags — CI checks out at depth 1, which is how a control quietly stops
# running.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-candidate-covers-main.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

pass=0
fail=0
ok() {
	printf '[check-candidate-covers-main-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-candidate-covers-main-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}
skip() {
	printf '[check-candidate-covers-main-test] SKIP %s\n' "$1"
}

WORK="$(mktemp -d)"
FIXTURE="$WORK/repo"
STUB="$WORK/surface-stub.sh"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# The stub stands in for check-artifact-unchanged.sh. It echoes what that script
# would and exits the code named in STUB_EXIT, so each verdict the subject has to
# map can be posed directly, including the exit 2 that means "could not measure".
cat >"$STUB" <<'STUB_BODY'
#!/usr/bin/env bash
set -euo pipefail
case "${STUB_EXIT:-0}" in
0) echo "check-artifact-unchanged: ok (stub, none on the released surface)"; exit 0 ;;
1)
	echo "check-artifact-unchanged: 1 file(s) on the released surface changed:" >&2
	echo "  cmd/agc/go.mod" >&2
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

LAST_OUT=""
# run_case DESC WANT_EXIT STUB_EXIT [ARGS...] — invoke the subject in the fixture.
run_case() {
	local desc="$1" want="$2" stub_exit="$3"
	shift 3
	local out rc=0
	out="$(cd "$FIXTURE" &&
		STUB_EXIT="$stub_exit" CHECK_CANDIDATE_SURFACE_CHECK="$STUB" \
			"$SUBJECT" "$@" 2>&1)" || rc=$?
		die_if_killed "$desc" "$rc" "$want"
	LAST_OUT="$out"
	if [[ "$rc" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc (want exit $want, got $rc)"
		printf '       %s\n' "$out" >&2
		LAST_OUT=""
	fi
}

# says DESC NEEDLE — the finding has to name things, not just exit non-zero.
says() {
	[[ "$LAST_OUT" == *"$2"* ]] || bad "$1 (output lacked '$2': $LAST_OUT)"
}

# Usage errors are exit 2, distinct from a finding, so a broken invocation cannot
# be read as "the candidate is spent".
usage_rc=0
(cd "$REPO_ROOT" && "$SUBJECT" one two >/dev/null 2>&1) || usage_rc=$?
die_if_killed "too many arguments is a usage error" "$usage_rc"
if [[ "$usage_rc" -eq 2 ]]; then
	ok "too many arguments is a usage error"
else
	bad "too many arguments is a usage error (want exit 2, got $usage_rc)"
fi

git init --quiet -b main "$FIXTURE"
# A fixture repo must not run background git maintenance: it outlives the suite
# and races the traces the gate reads. docs/development/testing.md#a-fixture-repo-must-not-run-background-git
fixture_git config maintenance.auto false
printf 'fixture\n' >"$FIXTURE/README.md"
fixture_git add -A
fixture_git commit --quiet -m 'chore: fixture base'

run_case "a ref that is not a commit is exit 2" 2 0 "definitely-not-a-ref"

# No prerelease tag at all: nothing is outstanding, so there is nothing to warn
# about however much the surface moved. The stub is armed to report a finding, so
# this asserts the tag state gates the surface check rather than the other way
# round — the subject must not even ask.
fixture_git tag v1.5.0
run_case "no candidate outstanding is quiet" 0 1
says "no-candidate case should say so" "no candidate outstanding"

# Cut a candidate. Now the surface check's verdict is what decides.
fixture_git tag v1.6.0-rc.1
run_case "a candidate that still covers main is quiet" 0 0
says "the covering case names the candidate" "v1.6.0-rc.1"

# The case the row was filed for: something on the released surface moved while a
# candidate was outstanding, and nothing said so until promote time.
run_case "a moved surface invalidates the outstanding candidate" 1 1
says "the finding names the candidate it invalidates" "v1.6.0-rc.1"
says "the finding lists the file that moved" "cmd/agc/go.mod"
says "the finding says it is not a broken build" "not a broken build"

# A delegate that could not measure must not be read as a finding. Exit 1 retires
# a candidate; a crash reported as 1 would retire one nothing ever measured.
run_case "a delegate that cannot measure is exit 2, not a finding" 2 2
run_case "an unexpected delegate status is exit 2" 2 7

# Cutting a newer candidate is the response the finding asks for, and it must
# quiet the watch rather than need a flag. Ordered before promotion deliberately,
# so no case deletes a tag — one that did would need a destructive-git approval on
# every `make scripts-test`.
fixture_git tag v1.6.0-rc.2
run_case "cutting a newer candidate resets the window" 0 0
says "the newest prerelease is the reference" "v1.6.0-rc.2"

# Promotion retires it. A watch still firing after the stable tag would report a
# candidate nobody is waiting on — and here the stub is armed to find something,
# so a subject that asked anyway would fail this.
fixture_git tag v1.6.0
run_case "promoting the release retires the warning" 0 1
says "after promotion the watch is quiet" "no candidate outstanding"

# The incident itself, end to end, with the real surface derivation: v1.6.0-rc.1
# outstanding and #1725 (811c670d5) landing 11 go.mod/go.sum files on top of it.
# Needs tags and history, so it skips on a shallow or tagless clone.
incident_ready() {
	git -C "$REPO_ROOT" rev-parse --verify --quiet 'v1.6.0-rc.1^{commit}' >/dev/null 2>&1 &&
		git -C "$REPO_ROOT" rev-parse --verify --quiet '811c670d5^{commit}' >/dev/null 2>&1
}
if incident_ready; then
	INCIDENT="$WORK/incident"
	# Tags are created rather than pruned: posing the window by deleting v1.6.0
	# would need a destructive-git approval on every run.
	git clone --shared --quiet --no-tags "$REPO_ROOT" "$INCIDENT"
	git -C "$INCIDENT" config maintenance.auto false
	git -C "$INCIDENT" tag v1.6.0-rc.1 "$(git -C "$REPO_ROOT" rev-parse 'v1.6.0-rc.1^{commit}')"
	inc_rc=0
	inc_out="$(cd "$INCIDENT" && "$SUBJECT" 811c670d5 2>&1)" || inc_rc=$?
	die_if_killed "the v1.6.0-rc.1 incident is caught end to end" "$inc_rc"
	if [[ "$inc_rc" -eq 1 && "$inc_out" == *"v1.6.0-rc.1"* && "$inc_out" == *"go.mod"* ]]; then
		ok "the v1.6.0-rc.1 incident is caught end to end"
	else
		bad "the v1.6.0-rc.1 incident is caught end to end (exit $inc_rc)"
		printf '       %s\n' "$inc_out" >&2
	fi
	# The negative from the same window: #1674 (0d9a40cbc) bumped Actions
	# versions and moved nothing that ships, so it must not raise the alarm.
	neg_rc=0
	(cd "$INCIDENT" && "$SUBJECT" 0d9a40cbc >/dev/null 2>&1) || neg_rc=$?
	die_if_killed "the same window's Actions bump moves nothing that ships" "$neg_rc"
	if [[ "$neg_rc" -eq 0 ]]; then
		ok "the same window's Actions bump moves nothing that ships"
	else
		bad "the same window's Actions bump moves nothing that ships (got exit $neg_rc)"
	fi
else
	skip "incident cases (v1.6.0-rc.1 or 811c670d5 absent — shallow or tagless clone)"
fi

printf '[check-candidate-covers-main-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
