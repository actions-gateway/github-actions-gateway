#!/usr/bin/env bash
# record-validated-candidate-test.sh — asserts the verdict record the dogfood gate
# writes, against a scripted `gh`.
#
# The subject makes one outward-facing write per validation run and its cost is
# asymmetric: a record that fails is recoverable by re-running it, while a record
# that lands on the wrong commit says a candidate was validated when it was not —
# which is the class Q879 exists to close. So the disagreement case and the
# annotated-tag dereference are asserted directly rather than discovered on a
# release night.
#
# `gh` is stubbed on PATH rather than shadowed as a function, so the real script
# runs end to end. Each case scripts the stub's answers through the environment and
# reads back the calls it logged.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/record-validated-candidate.sh"

pass=0
fail=0
ok() {
	printf '[record-validated-candidate-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[record-validated-candidate-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

WORK="$(mktemp -d)"
BIN="$WORK/bin"
GH_LOG="$WORK/gh.log"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

mkdir -p "$BIN"
# The stub answers the three reads the subject makes and logs every call.
#   STUB_TAG        "<type> <sha>" for git/ref/tags/<tag>, empty => 404
#   STUB_TAGOBJ     commit sha for git/tags/<sha>, empty => failure
#   STUB_EXISTING   sha already recorded, empty => 404 (nothing recorded)
#   STUB_POST_FAIL  non-empty => the create-ref call fails
cat >"$BIN/gh" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${GH_LOG}"
case "$*" in
*"git/ref/tags/"*)
	[[ -n "${STUB_TAG:-}" ]] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
	read -r t s <<<"${STUB_TAG}"
	printf '%s %s\n' "$t" "$s"
	;;
*"git/tags/"*)
	[[ -n "${STUB_TAGOBJ:-}" ]] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
	printf '%s\n' "${STUB_TAGOBJ}"
	;;
*"git/ref/validated/"*)
	[[ -n "${STUB_EXISTING:-}" ]] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
	printf '%s\n' "${STUB_EXISTING}"
	;;
*"-X POST"*)
	[[ -z "${STUB_POST_FAIL:-}" ]] || { echo "gh: Reference already exists (HTTP 422)" >&2; exit 1; }
	echo '{"ref":"created"}'
	;;
*)
	echo "gh stub: unexpected call: $*" >&2
	exit 99
	;;
esac
STUB_BODY
chmod +x "$BIN/gh"

LAST_OUT=""
# run_case DESC WANT_EXIT [VAR=VALUE...] -- ARGS...
run_case() {
	local desc="$1" want="$2"
	shift 2
	local -a env_args=()
	while [[ $# -gt 0 && "$1" != "--" ]]; do
		env_args+=("$1")
		shift
	done
	shift || true
	: >"$GH_LOG"
	local out rc=0
	out="$(PATH="$BIN:$PATH" GH_LOG="$GH_LOG" REPO=owner/name \
		env "${env_args[@]}" "$SUBJECT" "$@" 2>&1)" || rc=$?
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

want_gh() {
	if grep -qF -- "$2" "$GH_LOG"; then
		ok "$1"
	else
		bad "$1: no gh call matching '$2'"
		cat "$GH_LOG" >&2
	fi
}

want_no_gh() {
	if grep -qF -- "$2" "$GH_LOG"; then
		bad "$1: unexpected gh call matching '$2'"
		cat "$GH_LOG" >&2
	else
		ok "$1"
	fi
}

COMMIT=1111111111111111111111111111111111111111
TAGOBJ=2222222222222222222222222222222222222222
OTHER=3333333333333333333333333333333333333333

run_case "usage with no argument" 2 --
run_case "a stable tag is not a candidate" 2 -- v1.6.0
want_out "the non-candidate case says what it wanted" "expected vX.Y.Z-rc.N"

# The tag every release tag here actually is: annotated, so git/ref/tags names a
# TAG object and the commit is one dereference further on. Recording the tag
# object's sha would write a marker publish.yml cannot resolve to a commit.
run_case "an annotated tag is dereferenced to its commit" 0 \
	"STUB_TAG=tag ${TAGOBJ}" "STUB_TAGOBJ=${COMMIT}" -- v1.6.0-rc.2
want_gh "the tag object is dereferenced" "git/tags/${TAGOBJ}"
want_gh "the marker records the COMMIT, not the tag object" "sha=${COMMIT}"
want_gh "the marker is written under refs/validated" "ref=refs/validated/v1.6.0-rc.2"
want_out "the recorded ref is reported" "refs/validated/v1.6.0-rc.2 -> ${COMMIT}"

run_case "a lightweight tag needs no dereference" 0 \
	"STUB_TAG=commit ${COMMIT}" -- v1.6.0-rc.2
want_no_gh "no tag object is fetched for a lightweight tag" "git/tags/"
want_gh "the marker records the commit" "sha=${COMMIT}"

# Re-running has to stay free: a gate whose record failed on a network blip is
# re-run by hand, and so is one re-run for any other reason.
run_case "an already-recorded candidate is a no-op" 0 \
	"STUB_TAG=commit ${COMMIT}" "STUB_EXISTING=${COMMIT}" -- v1.6.0-rc.2
want_out "the no-op says what is already recorded" "already records ${COMMIT}"
want_no_gh "nothing is written on a re-run" "-X POST"

# The one case that is a finding rather than a retry: a verdict is not re-pointed.
run_case "a record naming a different commit is refused" 1 \
	"STUB_TAG=commit ${COMMIT}" "STUB_EXISTING=${OTHER}" -- v1.6.0-rc.2
want_out "both commits are printed" "recorded: ${OTHER}"
want_out "the refusal says a verdict is not re-pointed" "not re-pointed"
want_no_gh "a disagreement never overwrites" "-X POST"

run_case "a tag absent on the remote cannot be recorded" 1 -- v1.6.0-rc.2
want_out "the missing tag is named" "could not read tag v1.6.0-rc.2"

run_case "a tag object that will not dereference is reported" 1 \
	"STUB_TAG=tag ${TAGOBJ}" -- v1.6.0-rc.2
want_out "the dereference failure says so" "could not dereference"

# A failed create must hand back the exact re-run, because the alternative a
# maintainer reaches for is another hour of dogfood.
run_case "a failed create names the re-run" 1 \
	"STUB_TAG=commit ${COMMIT}" "STUB_POST_FAIL=1" -- v1.6.0-rc.2
want_out "the failure says the validation itself stands" "The validation itself is unaffected"
want_out "the failure prints the recorder command" "record-validated-candidate.sh v1.6.0-rc.2"

printf '[record-validated-candidate-test] %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
