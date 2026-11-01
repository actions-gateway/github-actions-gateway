#!/usr/bin/env bash
#
# Unit tests for scripts/fetch/backfill-image-content.sh (Q1242). The defect is
# a containerd-store daemon holding an image without its layer blobs, which
# needs two docker images sharing layers to reproduce, so the daemon is stubbed:
# these pin the decision the script makes on each save outcome. An image that
# exports is left alone, only the missing-platform error triggers the BuildKit
# fetch, the fetch goes to the daemon's own builder and through the registry
# mirror, and a fetch that does not fix the export fails loudly.
#
# docker and sleep are stubbed on PATH, so none of it touches a daemon, the
# network, or the clock. Runs under `make check` (via `make scripts-test`) and
# the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/fetch/backfill-image-content.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/backfill-image-content-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

assert_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$want" == "$got" ]]; then
		pass "$name"
	else
		fail "$name" "want $want got $got"
	fi
}

# docker: `context show` prints a builder name; `save --platform` fails with
# SAVE_ERROR until a buildx build has run, or for good when BUILD_FIXES is 0;
# `buildx build` records the Dockerfile its context holds and fails BUILD_FAILS
# times first.
cat > "$FIXTURE_DIR/bin/docker" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$STUB_DIR/docker.calls"
case "$1" in
context)
	echo stub-context
	;;
save)
	if [[ -n "${SAVE_ERROR:-}" ]] && { [[ ! -f "$STUB_DIR/built" ]] || ((${BUILD_FIXES:-1} == 0)); }; then
		echo "Error response from daemon: ${SAVE_ERROR}" >&2
		exit 1
	fi
	;;
buildx)
	n=$(($(cat "$STUB_DIR/build.count" 2> /dev/null || echo 0) + 1))
	echo "$n" > "$STUB_DIR/build.count"
	if ((n <= ${BUILD_FAILS:-0})); then
		exit 1
	fi
	cp "${!#}/Dockerfile" "$STUB_DIR/Dockerfile"
	: > "$STUB_DIR/built"
	;;
esac
STUB

cat > "$FIXTURE_DIR/bin/sleep" << 'STUB'
#!/usr/bin/env bash
STUB

chmod +x "$FIXTURE_DIR/bin/docker" "$FIXTURE_DIR/bin/sleep"
export STUB_DIR="$FIXTURE_DIR"
export PATH="$FIXTURE_DIR/bin:$PATH"
unset REGISTRY_MIRRORS SAVE_ERROR BUILD_FIXES BUILD_FAILS

missing='no suitable export target found: image with reference registry.example/app:v1 was found but does not provide the specified platform (linux/amd64)'

run_rc=0
builds=0
# run_backfill IMAGE... — run the script against clean counters, for linux/amd64.
run_backfill() {
	rm -f "$FIXTURE_DIR/docker.calls" "$FIXTURE_DIR/build.count" "$FIXTURE_DIR/built" "$FIXTURE_DIR/Dockerfile"
	: > "$FIXTURE_DIR/docker.calls"
	run_rc=0
	"$SCRIPT" linux/amd64 "$@" > /dev/null 2>&1 || run_rc=$?
	die_if_killed "backfill $*" "$run_rc"
	builds=$(grep -c '^buildx build' "$FIXTURE_DIR/docker.calls" || true)
}

# --- an image that exports is left alone ------------------------------------

run_backfill registry.example/app:v1
assert_eq 'an exporting image exits 0' 0 "$run_rc"
assert_eq 'an exporting image triggers no fetch' 0 "$builds"

# --- the missing-platform error triggers one fetch, then exports -------------

SAVE_ERROR="$missing" run_backfill registry.example/app:v1
assert_eq 'a backfilled image exits 0' 0 "$run_rc"
assert_eq 'a missing platform triggers one fetch' 1 "$builds"
build_line=$(grep '^buildx build' "$FIXTURE_DIR/docker.calls" || true)
case "$build_line" in
*'--builder stub-context '*'--platform linux/amd64 '*'type=docker,dest='*)
	pass "the fetch runs on the daemon's own builder, for the platform, with an export"
	;;
*) fail "the fetch runs on the daemon's own builder, for the platform, with an export" "got '$build_line'" ;;
esac
assert_eq 'the fetch builds FROM the image' 'FROM registry.example/app:v1' "$(cat "$FIXTURE_DIR/Dockerfile")"

# --- the fetch goes through the registry mirror ------------------------------

REGISTRY_MIRRORS='registry.example=mirror.example:5000' SAVE_ERROR="$missing" \
	run_backfill registry.example/app:v1
assert_eq 'a mirrored backfill exits 0' 0 "$run_rc"
assert_eq 'the fetch builds FROM the mirror ref' 'FROM mirror.example:5000/app:v1' "$(cat "$FIXTURE_DIR/Dockerfile")"

# --- a transient fetch failure is retried ------------------------------------

RETRY_DELAY=0 BUILD_FAILS=1 SAVE_ERROR="$missing" run_backfill registry.example/app:v1
assert_eq 'a fetch that fails once is retried to success' 0 "$run_rc"
assert_eq 'the retry makes a second fetch' 2 "$builds"

# --- any other save error fails without a fetch ------------------------------

SAVE_ERROR='No such image: registry.example/app:v1' run_backfill registry.example/app:v1
assert_eq 'another save error exits 1' 1 "$run_rc"
assert_eq 'another save error triggers no fetch' 0 "$builds"

# --- a fetch that does not fix the export fails loudly -----------------------

BUILD_FIXES=0 SAVE_ERROR="$missing" run_backfill registry.example/app:v1
assert_eq 'an export still missing after the fetch exits 1' 1 "$run_rc"

if ((fails > 0)); then
	printf '%d failure(s)\n' "$fails" >&2
	exit 1
fi
echo 'all backfill-image-content tests passed'
