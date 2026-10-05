#!/usr/bin/env bash
# fetch-gate-evidence-test.sh — asserts which CI runs the publish check takes its
# gate evidence from, against a scripted `gh`.
#
# The subject's one job is trust: an artifact counts only on a run of main's
# validate-candidate.yml dispatched on main. A run of a branch's copy, or of the
# tag push's dispatch job, can carry an artifact of the same name, and taking it
# would let a branch vouch for a candidate. So every field the subject filters on
# is posed wrong once here, and has to be refused.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
SUBJECT="$SCRIPT_DIR/fetch-gate-evidence.sh"

pass=0
fail=0
ok() {
	printf '[fetch-gate-evidence-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[fetch-gate-evidence-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

WORK="$(mktemp -d)"
BIN="$WORK/bin"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$BIN"

# The stub serves the run list from STUB_RUNS, each run's artifacts from
# STUB_ARTIFACTS_<run-id>, and a download by writing the run id into the file, so
# the test reads back which runs' evidence arrived.
#   STUB_LIST_FAIL      non-empty => the run list fails
#   STUB_DOWNLOAD_FAIL  a run id whose download fails
cat >"$BIN/gh" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
case "$*" in
"api --paginate "*"/runs?"*)
	[[ -z "${STUB_LIST_FAIL:-}" ]] || { echo "gh: HTTP 500" >&2; exit 1; }
	printf '%s\n' "${STUB_RUNS}"
	;;
"api "*"/actions/runs/"*"/artifacts --jq "*)
	run="${2#*/actions/runs/}"
	run="${run%%/*}"
	var="STUB_ARTIFACTS_${run}"
	jq -r "$4" <<<"${!var:-{\"artifacts\":[]\}}"
	;;
"run download "*)
	run="$3"
	[[ "${run}" != "${STUB_DOWNLOAD_FAIL:-}" ]] || { echo "gh: download failed" >&2; exit 1; }
	dir=""
	while (($#)); do
		[[ "$1" == --dir ]] && dir="$2"
		shift
	done
	[[ "${dir}" == /* ]] || { echo "gh stub: no absolute --dir" >&2; exit 98; }
	mkdir -p "${dir}"
	printf 'tag from-run-%s\n' "${run}" >"${dir}/gate-evidence.txt"
	;;
*)
	echo "gh stub: unexpected call: $*" >&2
	exit 99
	;;
esac
STUB_BODY
chmod +x "$BIN/gh"

run_json() { # run_json ID PATH BRANCH EVENT TITLE
	printf '{"id":%s,"path":"%s","head_branch":"%s","event":"%s","display_title":"%s"}' "$@"
}
WF=".github/workflows/validate-candidate.yml"
runs=(
	"$(run_json 101 "$WF" main workflow_dispatch "validate-candidate v1.9.0-rc.2")"
	"$(run_json 102 "$WF" main workflow_dispatch "validate-candidate v1.9.0-rc.1")"
	"$(run_json 201 ".github/workflows/other.yml" main workflow_dispatch "validate-candidate v1.9.0-rc.2")"
	"$(run_json 202 "$WF" claude/branch workflow_dispatch "validate-candidate v1.9.0-rc.2")"
	"$(run_json 203 "$WF" main push "validate-candidate v1.9.0-rc.2")"
	"$(run_json 204 "$WF" main workflow_dispatch "validate-candidate v1.8.0-rc.3")"
)
STUB_RUNS="$(IFS=,; printf '{"workflow_runs":[%s]}' "${runs[*]}")"
export STUB_RUNS
good='{"artifacts":[{"name":"gate-evidence-1","expired":false},{"name":"other","expired":false}]}'
export STUB_ARTIFACTS_101="$good" STUB_ARTIFACTS_201="$good" STUB_ARTIFACTS_202="$good" \
	STUB_ARTIFACTS_203="$good" STUB_ARTIFACTS_204="$good"
export STUB_ARTIFACTS_102='{"artifacts":[{"name":"gate-evidence-1","expired":true}]}'

OUT="$WORK/out"
run_subject() {
	rm -rf "$OUT"
	local rc=0
	PATH="$BIN:$PATH" REPO=owner/name "$SUBJECT" v1.9.0 "$OUT" >"$WORK/log" 2>&1 || rc=$?
	die_if_killed "$1" "$rc" "$2"
	if [[ "$rc" == "$2" ]]; then
		ok "$1 (exit ${rc})"
	else
		bad "$1: want exit $2, got ${rc}"
		cat "$WORK/log" >&2
	fi
}
fetched() { find "$OUT" -name gate-evidence.txt -exec cat {} + 2>/dev/null | sort | xargs; }

run_subject "a release line's evidence is fetched" 0
want="tag from-run-101"
got="$(fetched)"
if [[ "$got" == "$want" ]]; then
	ok "only run 101 counts: main's workflow, dispatched on main, this line, unexpired"
else
	bad "fetched evidence: want '${want}', got '${got}'"
fi
if [[ -e "$OUT/101-other" ]]; then
	bad "an artifact without the evidence prefix was downloaded"
else
	ok "an artifact without the evidence prefix is not downloaded"
fi

STUB_DOWNLOAD_FAIL=101 run_subject "a failed download is skipped, not fatal" 0
if [[ -z "$(fetched)" ]]; then
	ok "  ...and contributes nothing"
else
	bad "  ...a failed download contributed evidence: $(fetched)"
fi

STUB_LIST_FAIL=1 run_subject "a run list that cannot be read is exit 2" 2

rc=0
"$SUBJECT" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 2 ]]; then ok "usage with no argument (exit 2)"; else bad "usage: want exit 2, got ${rc}"; fi

printf '[fetch-gate-evidence-test] %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
