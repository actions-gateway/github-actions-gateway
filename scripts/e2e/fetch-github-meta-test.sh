#!/usr/bin/env bash
#
# Unit tests for scripts/e2e/fetch-github-meta.sh (Q1119). The script's job is to
# leave a usable /meta body or nothing, and to keep the token off argv. A caller
# treats a missing file as "no seed", so the failure modes that matter are the
# ones that would leave a file behind that the GMC could not use: a non-200
# answer, and a 200 with empty range lists. The token is the other half: a curl
# command line is visible to every process on the runner.
#
# `curl` is stubbed on PATH. The stub answers from STUB_STATUS and STUB_BODY and
# records its argv and the config file it was handed, so the assertions read what
# the script sent rather than what it printed.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/e2e/fetch-github-meta.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/fetch-github-meta-test.$$"
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
		fail "$name" "want '$want' got '$got'"
	fi
}

# curl: writes STUB_BODY to the --output path, prints STUB_STATUS as the
# --write-out value, and records argv plus the contents of any --config file.
cat > "$FIXTURE_DIR/bin/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" > "$STUB_DIR/argv"
out="" cfg=""
while (($#)); do
	case "$1" in
		--output) out="$2"; shift ;;
		--config) cfg="$2"; shift ;;
	esac
	shift
done
[[ -n "$cfg" ]] && cat "$cfg" > "$STUB_DIR/config"
printf '%s' "$STUB_BODY" > "$out"
printf '%s' "$STUB_STATUS"
STUB
chmod +x "$FIXTURE_DIR/bin/curl"

export STUB_DIR="$FIXTURE_DIR"
export PATH="$FIXTURE_DIR/bin:$PATH"

readonly GOOD_BODY='{"api":["192.30.252.0/22"],"actions":["4.148.0.0/16"],"web":["140.82.112.0/20"]}'
readonly SECRET='ghs_test_token_value_not_real'
out="$FIXTURE_DIR/meta.json"

# run_fetch — run the script, leaving its status in run_rc and its output in
# run_out. GH_TOKEN is the caller's to set or unset.
run_rc=0
run_out=""
run_fetch() {
	rm -f "$out" "$FIXTURE_DIR/argv" "$FIXTURE_DIR/config"
	run_rc=0
	run_out="$("$SCRIPT" "$out" 2>&1)" || run_rc=$?
}

# --- success: a 200 with all three lists writes the body ---------------------

GH_TOKEN="$SECRET" STUB_STATUS=200 STUB_BODY="$GOOD_BODY" run_fetch
assert_eq 'a complete 200 exits 0' 0 "$run_rc"
assert_eq 'a complete 200 writes the body' "$GOOD_BODY" "$(cat "$out" 2> /dev/null || true)"

# --- the token reaches curl through a config file, never argv or output -------

if grep -q -F -- "$SECRET" "$FIXTURE_DIR/argv"; then
	fail 'token absent from argv' 'the token appears on the curl command line'
else
	pass 'token absent from argv'
fi
if grep -q -F -- "Authorization: Bearer $SECRET" "$FIXTURE_DIR/config"; then
	pass 'token delivered in the config file'
else
	fail 'token delivered in the config file' 'no Authorization header in the config'
fi
if [[ "$run_out" == *"$SECRET"* ]]; then
	fail 'token absent from output' 'the token was printed'
else
	pass 'token absent from output'
fi

# --- failure modes leave no file ---------------------------------------------

GH_TOKEN="$SECRET" STUB_STATUS=403 STUB_BODY='{"message":"API rate limit exceeded"}' run_fetch
assert_eq 'a 403 exits non-zero' 1 "$run_rc"
assert_eq 'a 403 leaves no file' no "$([[ -e "$out" ]] && echo yes || echo no)"

GH_TOKEN="$SECRET" STUB_STATUS=200 STUB_BODY='{"api":[],"actions":["4.148.0.0/16"],"web":[]}' run_fetch
assert_eq 'a 200 with an empty range list exits non-zero' 1 "$run_rc"
assert_eq 'a 200 with an empty range list leaves no file' no "$([[ -e "$out" ]] && echo yes || echo no)"

GH_TOKEN="$SECRET" STUB_STATUS=200 STUB_BODY='not json' run_fetch
assert_eq 'a 200 that is not JSON exits non-zero' 1 "$run_rc"

# --- no token: refuse the unauthenticated call, and never reach curl ---------

rm -f "$FIXTURE_DIR/argv" "$out"
env -u GH_TOKEN STUB_STATUS=200 STUB_BODY="$GOOD_BODY" bash -c "
	rc=0; '$SCRIPT' '$out' > /dev/null 2>&1 || rc=\$?; exit \$rc" && no_token_rc=0 || no_token_rc=$?
assert_eq 'a missing token exits non-zero' 1 "$no_token_rc"
assert_eq 'a missing token never calls curl' no "$([[ -e "$FIXTURE_DIR/argv" ]] && echo yes || echo no)"

if ((fails > 0)); then
	printf '%d failure(s)\n' "$fails" >&2
	exit 1
fi
