#!/usr/bin/env bash
# check-vuln-policy-test.sh — asserts the offline helpers of check-vuln-policy.sh in
# both directions: a run that must count, and each shape that must not.
#
# Job names and the `Total:` line are the shapes security-scan.yml run 44106603383
# produced on 2026-11-17, including the matrix exit-code inside a trivy leg's name.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
CHECK_VULN_POLICY_LIB=1 source "$SCRIPT_DIR/check-vuln-policy.sh"

pass=0
fail=0
# expect NAME WANT GOT
expect() {
	if [[ "$2" == "$3" ]]; then
		printf '[check-vuln-policy-test] ok   %s\n' "$1"
		pass=$((pass + 1))
	else
		printf '[check-vuln-policy-test] FAIL %s\n       want: %q\n       got:  %q\n' "$1" "$2" "$3" >&2
		fail=$((fail + 1))
	fi
}

NOW=1794931200 # 2026-11-17T16:00:00Z
RUNS='[
 {"databaseId":3,"event":"schedule","conclusion":"success","createdAt":"2026-11-17T14:45:27Z"},
 {"databaseId":2,"event":"merge_group","conclusion":"failure","createdAt":"2026-11-17T15:00:00Z"},
 {"databaseId":1,"event":"push","conclusion":"success","createdAt":"2026-11-15T10:00:00Z"}
]'
expect fresh-run-counts "3	schedule	2026-11-17T14:45:27Z" "$(fresh_runs "$RUNS" "$NOW" 24)"
expect failed-run-never-counts "" "$(fresh_runs "$RUNS" "$NOW" 0)"
expect old-run-counts-in-a-wider-window "3,1" "$(fresh_runs "$RUNS" "$NOW" 72 | cut -f1 | paste -sd, -)"

FULL='{"jobs":[
 {"name":"changes","conclusion":"success"},
 {"name":"govulncheck","conclusion":"success"},
 {"id":11,"name":"trivy (gmc, 1)","conclusion":"success"},
 {"id":12,"name":"trivy (worker, 0)","conclusion":"success"},
 {"id":13,"name":"trivy (build-runner, 0)","conclusion":"success"},
 {"name":"polaris","conclusion":"skipped"}
]}'
expect full-scan-has-no-gaps "" "$(scan_gaps "$FULL")"

SKIPPED='{"jobs":[{"name":"govulncheck","conclusion":"skipped"},{"name":"trivy","conclusion":"skipped"}]}'
expect path-skipped-scan-is-a-gap "govulncheck: skipped, trivy: skipped" "$(scan_gaps "$SKIPPED")"

RED='{"jobs":[{"name":"govulncheck","conclusion":"success"},{"name":"trivy (agc, 1)","conclusion":"failure"}]}'
expect red-blocking-leg-is-a-gap "trivy (agc, 1): failure" "$(scan_gaps "$RED")"

expect empty-run-is-a-gap "govulncheck: absent, trivy: absent" "$(scan_gaps '{"jobs":[]}')"

expect report-only-legs-by-exit-code "12	worker,13	build-runner" "$(report_only_legs "$FULL" | paste -sd, -)"

LOG='2026-11-17T14:47:39.37Z Node.js (node-pkg)
2026-11-17T14:47:39.37Z Total: 28 (HIGH: 27, CRITICAL: 1)
2026-11-17T14:47:39.37Z usr/local/lib/docker/cli-plugins/docker-compose (gobinary)
2026-11-17T14:47:39.37Z Total: 1 (HIGH: 1, CRITICAL: 0)'
expect fixable-total-sums-targets "29" "$(fixable_total "$LOG")"
expect fixable-total-of-clean-log "0" "$(fixable_total 'no findings')"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
printf 'FROM scratch AS wrapper\nFROM ghcr.io/actions/actions-runner:2.338.2@sha256:abc AS worker\n' >"$WORK/Dockerfile"
expect runner-pin-from-dockerfile "2.338.2" "$(dockerfile_runner_pin "$WORK/Dockerfile")"
printf 'FROM scratch AS wrapper\n' >"$WORK/Dockerfile"
expect no-runner-pin-is-empty "" "$(dockerfile_runner_pin "$WORK/Dockerfile")"

# Ignore entries: bounded ones pass, and each way of being unbounded is named.
cat >"$WORK/ok.yaml" <<'YAML'
vulnerabilities:
  - id: CVE-2026-1
    paths:
      - usr/local/bin/docker
    statement: waits on docker 29.8
    expired_at: 2026-12-17
YAML
expect bounded-entry-is-read "CVE-2026-1	2026-12-17	waits on docker 29.8" "$(ignore_entries "$WORK/ok.yaml")"
expect bounded-entry-has-no-gaps "" "$(ignore_gaps "$WORK/ok.yaml" "$NOW")"
sed 's/2026-12-17/2027-03-01/' "$WORK/ok.yaml" >"$WORK/far.yaml"
expect far-expiry-is-a-gap "CVE-2026-1: expires 2027-03-01, more than 60 days out" "$(ignore_gaps "$WORK/far.yaml" "$NOW")"
grep -v expired_at "$WORK/ok.yaml" >"$WORK/noexp.yaml"
expect missing-expiry-is-a-gap "CVE-2026-1: no expired_at" "$(ignore_gaps "$WORK/noexp.yaml" "$NOW")"
grep -v statement "$WORK/ok.yaml" >"$WORK/nost.yaml"
expect missing-statement-is-a-gap "CVE-2026-1: no statement" "$(ignore_gaps "$WORK/nost.yaml" "$NOW")"
printf 'vulnerabilities:\n' >"$WORK/empty.yaml"
expect empty-list-is-a-gap "no entries read from empty.yaml" "$(ignore_gaps "$WORK/empty.yaml" "$NOW")"
printf 'vulnerabilities: []\n' >"$WORK/inline.yaml"
expect inline-empty-list-fails-closed "unsupported layout, line 1: vulnerabilities: []" "$(ignore_gaps "$WORK/inline.yaml" "$NOW")"

# Layouts trivy reads and this parser does not must fail closed, never drop out:
# an entry that cannot be read cannot be bounded.
cat >"$WORK/swapped.yaml" <<'YAML'
vulnerabilities:
  - id: CVE-A
    statement: a
    expired_at: 2026-12-01
  - statement: b
    id: CVE-B
    expired_at: 2030-01-01
YAML
expect swapped-key-order-fails-closed "unsupported layout, line 5:   - statement: b" "$(ignore_gaps "$WORK/swapped.yaml" "$NOW" | head -1)"
expect swapped-entry-keeps-neighbour-intact "" "$(ignore_gaps "$WORK/swapped.yaml" "$NOW" | grep CVE-A || true)"
printf 'vulnerabilities:\n  - {id: CVE-C, expired_at: 2030-01-01}\n' >"$WORK/flow.yaml"
expect flow-style-fails-closed "unsupported layout, line 2:   - {id: CVE-C, expired_at: 2030-01-01}" "$(ignore_gaps "$WORK/flow.yaml" "$NOW")"
sed 's/statement: waits on docker 29.8/statement: >-/' "$WORK/ok.yaml" >"$WORK/block.yaml"
expect block-scalar-statement-fails-closed "unsupported layout, line 5:     statement: >-" "$(ignore_gaps "$WORK/block.yaml" "$NOW" | head -1)"
sed 's/expired_at: 2026-12-17/expired_at: "2026-12-17"  # re-check then/' "$WORK/ok.yaml" >"$WORK/quoted.yaml"
expect quoted-commented-expiry-reads "" "$(ignore_gaps "$WORK/quoted.yaml" "$NOW")"
{ cat "$WORK/ok.yaml"; printf '    purls:\n      - pkg:golang/x\n'; } >"$WORK/unknown.yaml"
expect unknown-key-fails-closed "unsupported layout, line 7:     purls:" "$(ignore_gaps "$WORK/unknown.yaml" "$NOW" | head -1)"

# Every committed ignore file must parse whole: at least one entry, each with a
# statement, and no line outside the layout ignore_entries reads.
for f in "$SCRIPT_DIR"/../security/trivyignore-*.yaml; do
	[[ -f "$f" ]] || continue
	n="$(ignore_entries "$f" | awk -F'\t' '$1 != "!" && $3 != "-"' | wc -l | tr -d ' ')"
	expect "committed-$(basename "$f")-parses" 1 "$((n > 0))"
	expect "committed-$(basename "$f")-has-no-unread-lines" "" "$(ignore_entries "$f" | grep '^!' || true)"
done

# The real Dockerfile must still carry a pin the helper can read, or check 3 is
# UNMEASURED on every release.
expect real-dockerfile-has-a-pin 1 "$(dockerfile_runner_pin "$SCRIPT_DIR/../../Dockerfile" | grep -cE '^[0-9]+\.[0-9]+\.[0-9]+$' || true)"

printf '[check-vuln-policy-test] %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
