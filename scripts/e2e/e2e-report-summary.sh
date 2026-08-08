#!/usr/bin/env bash
#
# Render the e2e JUnit report into the job summary and failure annotations (Q608).
#
# `make e2e` has written tmp/e2e-report.xml since the suite was parallelized, and
# the workflow has uploaded it as an artifact ever since — but nothing rendered
# it: the upload was planned on the premise that Actions draws a test-summary
# table from it via a built-in reporter, and there is no such reporter — so
# reading the result of a failed run still meant downloading a 300 KB XML by
# hand. This closes that gap (Q608).
#
# Parsing is awk, not an XML library: no XML tool is in the required tier of
# scripts/ci/check-tools.sh, and this must run wherever `make e2e` ran. That is
# safe here because the input is encoding/xml's own output — one <testcase> per
# line, every attribute value XML-escaped, so an attribute ends at the next
# literal quote.
#
# Usage:
#   scripts/e2e/e2e-report-summary.sh [REPORT]     # default tmp/e2e-report.xml
#
# Writes markdown to $GITHUB_STEP_SUMMARY (stdout when unset) and ::error::
# annotations to stdout. Never fails the step: a missing or malformed report is
# reported as such, because this runs on the failure path where the suite may
# have died before writing anything.
#
# Sourcing this file defines its helpers without rendering anything, which is how
# e2e-report-summary-test.sh asserts them.
set -euo pipefail
shopt -s inherit_errexit

REPORT_DEFAULT="tmp/e2e-report.xml"

# Slowest-spec rows in the summary. The list exists to aim speed work, so it
# wants to be long enough to show a pattern and short enough to read at a glance.
SUMMARY_SLOWEST="${E2E_SUMMARY_SLOWEST:-10}"

# parse_report FILE — normalize the JUnit XML to one TSV row per testcase:
#
#   status <TAB> seconds <TAB> name <TAB> message
#
# Every other function here derives from this, so the XML shape is understood in
# exactly one place. Ginkgo emits suite-level nodes ([SynchronizedBeforeSuite]
# and friends) as testcases alongside real specs; they are kept, because a
# failure in one is a suite failure that has to surface.
parse_report() {
	local file="$1"
	[[ -r "$file" ]] || return 0

	awk '
		# encoding/xml writes quotes and apostrophes as NUMERIC references
		# (&#34;, &#39;) rather than the named &quot;/&apos;, and whitespace as
		# &#xA;/&#xD;/&#x9;. Handling only the named forms leaves raw &#34; in
		# every spec name that quotes something.
		function unescape(s) {
			gsub(/&#xA;|&#xD;|&#x9;/, " ", s)
			gsub(/&#34;|&quot;/, "\"", s)
			gsub(/&#39;|&apos;/, "'"'"'", s)
			gsub(/&lt;/, "<", s)
			gsub(/&gt;/, ">", s)
			gsub(/&amp;/, "\\&", s)   # last: an unescaped & must not re-trigger the rules above
			gsub(/\t/, " ", s)        # the row separator can never appear inside a field
			return s
		}
		# attr(line, name) — value of name="..." in line, or "" when absent.
		function attr(line, name,   re, rest, v) {
			re = name "=\""
			if (match(line, re) == 0) return ""
			rest = substr(line, RSTART + RLENGTH)
			v = substr(rest, 1, index(rest, "\"") - 1)
			return unescape(v)
		}
		/<testcase / {
			if (name != "") print status "\t" secs "\t" name "\t" msg
			name = attr($0, "name")
			status = attr($0, "status")
			secs = attr($0, "time")
			msg = ""
		}
		# A spec carries at most one of these, and the message attribute holds the
		# whole failure on one line (newlines arrive as &#xA;).
		/<failure |<error / {
			if (msg == "") msg = attr($0, "message")
		}
		END { if (name != "") print status "\t" secs "\t" name "\t" msg }
	' "$file"
}

# count_status TSV STATUS — rows matching STATUS.
count_status() {
	local tsv="$1" status="$2"
	printf '%s\n' "$tsv" | awk -F'\t' -v s="$status" '$1 == s { n++ } END { print n + 0 }'
}

# failing_rows TSV — rows whose status is any of Ginkgo's failure states.
# Selecting the complement of the known-good states (rather than listing the
# failure states) means a state added upstream surfaces as a failure instead of
# vanishing from both columns.
failing_rows() {
	printf '%s\n' "$1" | awk -F'\t' '$1 != "passed" && $1 != "skipped" && $1 != "pending" && $1 != "" '
}

# slowest_rows TSV N — the N slowest real specs, slowest first. Suite-level
# nodes are excluded: they are setup cost, not a spec anyone can speed up.
slowest_rows() {
	printf '%s\n' "$1" | awk -F'\t' '$3 ~ /^\[It\]/' | sort -t"$(printf '\t')" -k2 -rn | head -n "$2"
}

# summarize_report FILE — the markdown block for $GITHUB_STEP_SUMMARY.
summarize_report() {
	local file="$1" tsv passed failed skipped

	echo '## e2e results'
	echo

	if [[ ! -r "$file" ]]; then
		echo "No report at \`$file\` — the suite exited before writing one."
		return 0
	fi

	tsv="$(parse_report "$file")"
	if [[ -z "$tsv" ]]; then
		echo "Report at \`$file\` has no testcases — the suite exited before running specs."
		return 0
	fi

	passed="$(count_status "$tsv" passed)"
	skipped="$(($(count_status "$tsv" skipped) + $(count_status "$tsv" pending)))"
	failed="$(failing_rows "$tsv" | grep -c . || true)"

	echo "**$passed passed** · **$failed failed** · **$skipped skipped**"
	echo

	if ((failed > 0)); then
		echo '### Failures'
		echo
		while IFS=$'\t' read -r _ _ name msg; do
			[[ -n "$name" ]] || continue
			echo "- **$name**"
			[[ -n "$msg" ]] && echo "  - $msg"
		done < <(failing_rows "$tsv")
		echo
	fi

	echo "### Slowest $SUMMARY_SLOWEST specs"
	echo
	echo '| Seconds | Spec |'
	echo '|---:|---|'
	while IFS=$'\t' read -r _ secs name _; do
		[[ -n "$name" ]] || continue
		printf '| %.0f | %s |\n' "$secs" "$name"
	done < <(slowest_rows "$tsv" "$SUMMARY_SLOWEST")
}

# annotate_failures FILE — one ::error:: per failed spec, so failures land at the
# top of the run instead of thousands of log lines down.
annotate_failures() {
	local file="$1" tsv
	[[ -r "$file" ]] || return 0
	tsv="$(parse_report "$file")"
	[[ -n "$tsv" ]] || return 0

	while IFS=$'\t' read -r _ _ name msg; do
		[[ -n "$name" ]] || continue
		# Annotation values are comma/newline delimited, so both are stripped.
		printf '::error title=%s::%s\n' \
			"${name//,/ }" \
			"$(printf '%s' "${msg:-spec failed}" | tr '\n,' '  ')"
	done < <(failing_rows "$tsv")
}

main() {
	local file="${1:-$REPORT_DEFAULT}"
	annotate_failures "$file"
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		summarize_report "$file" >>"$GITHUB_STEP_SUMMARY"
	else
		summarize_report "$file"
	fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
