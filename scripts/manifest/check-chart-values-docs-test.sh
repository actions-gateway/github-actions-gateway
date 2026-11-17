#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-chart-values-docs.py — the gate holding each
# chart README's values table to its values.yaml.
#
# The shipped tree passes, and so would a checker that had stopped reading
# anything, so every case breaks a copy of the real files and demands the verdict
# change: a key added to values.yaml with no row, a row left behind by a removed
# key, and three unreadable inputs that must refuse rather than pass.
#
# The expected-output needles quote backticked key names, which must stay literal.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
CHECKER="${REPO_ROOT}/scripts/manifest/check-chart-values-docs.py"
CHART=charts/actions-gateway

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the chart's values.yaml and README under a throwaway root, printed.
fixture() {
	local root="${WORKDIR}/case.$$.${RANDOM}"
	mkdir -p "${root}/${CHART}"
	cp "${REPO_ROOT}/${CHART}/values.yaml" "${REPO_ROOT}/${CHART}/README.md" "${root}/${CHART}/"
	printf '%s' "${root}"
}

# run_checker ROOT — run the gate against a fixture root, capturing rc and output.
run_checker() {
	rc=0
	out="$( (cd "$1" && python3 "${CHECKER}") 2>&1 )" || rc=$?
}

expect() {
	local name="$1" want_rc="$2" needle="$3"
	die_if_killed "$name" "$rc" "$want_rc"
	if [[ "${rc}" != "${want_rc}" ]]; then
		echo "FAIL ${name}: want rc ${want_rc}, got ${rc}" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	if [[ -n "${needle}" && "${out}" != *"${needle}"* ]]; then
		echo "FAIL ${name}: '${needle}' not in output" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	echo "ok   ${name}"
}

# drop_line FILE PATTERN — delete the lines matching PATTERN, failing the case if none matched.
drop_line() {
	local file="$1" pattern="$2" before after
	before="$(wc -l < "${file}")"
	awk -v p="${pattern}" '$0 !~ p' "${file}" > "${file}.new" && mv "${file}.new" "${file}"
	after="$(wc -l < "${file}")"
	if (( before == after )); then
		echo "FAIL fixture: pattern '${pattern}' matched nothing in ${file}" >&2
		exit 1
	fi
}

root="$(fixture)"
run_checker "${root}"
expect "shipped chart passes" 0 "ok (1 chart README(s)"

root="$(fixture)"
printf '\nnewSetting: true\n' >> "${root}/${CHART}/values.yaml"
run_checker "${root}"
expect "an undocumented values key fails" 1 'values key `newSetting` has no row'

root="$(fixture)"
drop_line "${root}/${CHART}/values.yaml" '^fqdnPolicyBackend:'
run_checker "${root}"
expect "a row for a removed key fails" 1 'row names `fqdnPolicyBackend`'

root="$(fixture)"
drop_line "${root}/${CHART}/values.yaml" '^tolerations:'
run_checker "${root}"
expect "a removed key sharing a row still fails" 1 'row names `tolerations`'

root="$(fixture)"
drop_line "${root}/${CHART}/README.md" '^## Values$'
run_checker "${root}"
expect "a README with no values table fails" 1 'no `## Values` table'

root="$(fixture)"
drop_line "${root}/${CHART}/README.md" '^[|] `'
run_checker "${root}"
expect "a values table with no rows refuses" 2 "parsed to no rows"

root="${WORKDIR}/empty"
mkdir -p "${root}"
run_checker "${root}"
expect "no charts refuses" 2 "no charts/*/values.yaml"

if (( fails > 0 )); then
	echo "${fails} case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
