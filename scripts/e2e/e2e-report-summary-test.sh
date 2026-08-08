#!/usr/bin/env bash
#
# Assertions for scripts/e2e/e2e-report-summary.sh's JUnit parsing (Q608).
#
# The fixture below is verbatim output from Ginkgo's own JUnit writer
# (reporters.GenerateJUnitReport), not hand-written XML. That distinction is the
# point of the file: encoding/xml escapes quotes as the NUMERIC &#34; rather than
# the named &quot;, and a parser written against hand-rolled XML passes its own
# tests while leaving raw entities in every spec name in the job summary.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/e2e/e2e-report-summary.sh
source "$REPO_ROOT/scripts/e2e/e2e-report-summary.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0

ok() { printf 'ok   %-46s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-46s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

want_contains() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == *"$want"* ]]; then
		ok "$name" "found $(printf '%q' "$want")"
	else
		bad "$name" "want substring $(printf '%q' "$want") in $(printf '%q' "$got")"
	fi
}

want_absent() {
	local name="$1" unwanted="$2" got="$3"
	if [[ "$got" != *"$unwanted"* ]]; then
		ok "$name" "absent $(printf '%q' "$unwanted")"
	else
		bad "$name" "should not contain $(printf '%q' "$unwanted"): $(printf '%q' "$got")"
	fi
}

REPORT="$WORK/report.xml"
cat >"$REPORT" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
  <testsuites tests="4" disabled="0" errors="0" failures="2" time="42">
      <testsuite name="e2e suite" package="/repo/cmd/gmc/test/e2e" tests="4" disabled="0" skipped="0" errors="0" failures="2" time="42" timestamp="0001-01-01T00:00:00">
          <properties>
              <property name="SuiteSucceeded" value="false"></property>
          </properties>
          <testcase name="[It] E2E_GMC_Isolation cross-tenant &#34;curl&#34; is denied &lt;by&gt; NetworkPolicy &amp; stays denied" classname="e2e suite" status="failed" time="12">
              <failure message="Expected&#xA;    &lt;int&gt;: 1&#xA;to equal&#xA;    &lt;int&gt;: 0&#xA;commas, too" type="failed">[FAILED] Expected&#xA;In [It] at: isolation_test.go:88 @ 01/01/01 00:00:00&#xA;</failure>
              <system-err>[FAILED] Expected&#xA;</system-err>
          </testcase>
          <testcase name="[It] E2E_AGC_WorkerDrain a drained worker is replaced" classname="e2e suite" status="timedout" time="300">
              <failure message="Timed out after 300.000s" type="timedout">[TIMEDOUT] Timed out after 300.000s&#xA;</failure>
              <system-err>[TIMEDOUT] Timed out after 300.000s&#xA;</system-err>
          </testcase>
          <testcase name="[It] E2E_GMC_Provisioning namespace is created" classname="e2e suite" status="passed" time="5"></testcase>
          <testcase name="[SynchronizedBeforeSuite]" classname="e2e suite" status="passed" time="22"></testcase>
      </testsuite>
  </testsuites>
EOF

echo '== XML entities are decoded, in the form encoding/xml actually writes =='
tsv="$(parse_report "$REPORT")"
want_contains 'numeric &#34; becomes a quote' 'cross-tenant "curl" is denied' "$tsv"
want_contains '&lt;/&gt; become angle brackets' 'denied <by> NetworkPolicy' "$tsv"
want_contains '&amp; becomes a bare ampersand' 'NetworkPolicy & stays denied' "$tsv"
want_absent 'no entity survives decoding' '&#' "$tsv"

echo
echo '== every failure state is a failure, and setup nodes are still counted =='
# `timedout` is the state a hung spec gets, and it is the one most worth seeing.
# Counting only status="failed" would report this run as 3 passed, 1 failed.
summary="$(summarize_report "$REPORT")"
want_contains 'passed/failed/skipped counts' '**2 passed** · **2 failed** · **0 skipped**' "$summary"
want_contains 'the failed spec is listed' '- **[It] E2E_GMC_Isolation cross-tenant "curl"' "$summary"
want_contains 'the timed-out spec is listed' '- **[It] E2E_AGC_WorkerDrain a drained worker is replaced**' "$summary"
want_contains 'the failure message is shown' 'to equal     <int>: 0' "$summary"

echo
echo '== the slowest table ranks real specs only =='
# [SynchronizedBeforeSuite] is 22 s of cluster bring-up. Leaving it in the table
# aims speed work at a spec nobody wrote.
slowest="$(slowest_rows "$tsv" 10)"
want_contains 'slowest first' '300' "$slowest"
want_absent 'suite-level nodes excluded' 'SynchronizedBeforeSuite' "$slowest"

echo
echo '== annotations survive titles and messages containing delimiters =='
# ::error title=X::Y is comma- and newline-delimited, so an unstripped message
# truncates the annotation at the first comma in a Gomega diff.
annotations="$(annotate_failures "$REPORT")"
want_contains 'one annotation per failure' '::error title=[It] E2E_AGC_WorkerDrain' "$annotations"
want_absent 'commas stripped from the message' 'commas, too' "$annotations"
want_contains 'message still present, comma replaced' 'commas  too' "$annotations"

echo
echo '== a missing or empty report is reported, not fatal =='
# This runs on the failure path, where the suite may have died before writing.
missing="$(summarize_report "$WORK/nope.xml")"
want_contains 'missing report explains itself' 'the suite exited before writing one' "$missing"
: >"$WORK/empty.xml"
empty="$(summarize_report "$WORK/empty.xml")"
want_contains 'empty report explains itself' 'exited before running specs' "$empty"
if annotate_failures "$WORK/nope.xml" >/dev/null 2>&1; then
	ok 'annotating a missing report' 'exits 0'
else
	bad 'annotating a missing report' 'should have exited 0'
fi

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
