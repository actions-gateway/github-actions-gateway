#!/usr/bin/env bash
#
# Behavioural tests for the workflow `run:` bodies that ACT (Q1006): the steps
# that open an issue or push a branch, driven with stubs on PATH and asserted on
# what the stubs were asked to do.
#
# WHY THESE STEPS AND NOT EVERY run: BODY.
# `make check` runs targets over the tree and no target enters a workflow step's
# shell, so a `run:` body is first executed by the runner on the event that
# triggers it. Eleven of the thirty workflows carry no `pull_request` trigger, so
# their bodies first run after merge — or at a `v*` tag for publish.yml. Neither
# actionlint nor shellcheck's integration executes a body, so a defect in what
# the script DECIDES is invisible to both: release-freeze-watch.yml reached
# review classifying its delegate's crash codes with `-eq 2` where they are 2 and
# above, so 7, 126 and 137 each fell through to the report step and opened an
# issue retiring a candidate nothing had measured. All of actionlint, shellcheck
# and `make check` were green, and it had a full review.
#
# Covering all thirty is not proportionate, so the subject is the steps that act
# rather than report — those that open an issue, push, publish or tag — minus the
# ones a `pull_request` event already executes. That derivation, and what it
# excludes, is in docs/development/testing.md § Driving a workflow `run:` body.
# Two steps qualify today:
#
#   release-freeze-watch.yml  `check` + `report`  — opens/comments/closes an issue
#   pages.yml                 `mike`              — pushes the gh-pages version tree
#
# THE POSITIVE CONTROLS ARE THE LOAD-BEARING HALF. A suite of "must not act"
# cases passes trivially for a body that has stopped acting at all, so each
# subject also carries a case that must STILL act: an rc=1 check whose report
# opens an issue, and a highest-release deploy that claims `stable` and pushes.
# The `regression` cases re-apply the historical defect to the extracted body and
# require the assertions to go red, so the suite is known to be able to fail.
#
# Runs under `make check` (via `make scripts-test`) and the CI scripts job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
EXTRACT="$REPO_ROOT/scripts/ci/workflow-step-body.sh"

FREEZE_WORKFLOW=".github/workflows/release-freeze-watch.yml"
PAGES_WORKFLOW=".github/workflows/pages.yml"

WORK="$REPO_ROOT/tmp/workflow-acting-steps.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT INT TERM

fails=0

fail() {
	printf 'FAIL %-34s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

pass() { printf 'ok   %-34s %s\n' "$1" "$2"; }

# refuse MESSAGE — exit 2 for anything that would leave this suite driving
# nothing. A body that failed to extract runs clean, and every case built on it
# would report green while asserting about an empty file.
refuse() {
	printf 'workflow-acting-steps: %s\n' "$1" >&2
	exit 2
}

# --- the shell these bodies actually get ------------------------------------
#
# GitHub's default `run:` shell is `bash --noprofile --norc -e {0}`; `-o
# pipefail` arrives only with an explicit `shell: bash`, a job `defaults.run.shell`
# or a workflow-level one. This repo sets none of the three, so driving with
# pipefail would model a shell no step here runs under — and not academically:
# measured 2026-09-04 on bash 5.3.15, the report step's
# `printf | sed | head -1` candidate extraction exits 141 under `-e -o pipefail`
# for a log long enough that `head` closes the pipe first, killing the step
# before it can open the issue, and exits 0 under `-e`. So the shell is asserted
# rather than assumed: a `shell:` or `defaults:` key appearing in either subject
# means this constant is stale and the suite refuses.
ACTIONS_SHELL=(bash --noprofile --norc -e)

for wf in "$FREEZE_WORKFLOW" "$PAGES_WORKFLOW"; do
	[[ -f "$wf" ]] || refuse "$wf does not exist, so there are no bodies to drive"
	if grep -nE '^[[:space:]]*(shell|defaults):' "$wf" >/dev/null; then
		refuse "$wf now sets shell: or defaults:, so \`${ACTIONS_SHELL[*]}\` may no longer be the shell its steps get — re-derive it from GitHub's defaults before trusting this suite"
	fi
done

# --- extract the bodies -----------------------------------------------------
#
# Verbatim from the tracked workflows, so a step renamed or rewritten fails here
# rather than leaving the cases below asserting against a stale copy.
extract() {
	local out="$WORK/$3.sh"
	"$EXTRACT" "$1" "$2" >"$out" || refuse "could not extract \"$2\" from $1"
	printf '%s\n' "$out"
}

CHECK_BODY="$(extract "$FREEZE_WORKFLOW" check freeze-check)"
REPORT_BODY="$(extract "$FREEZE_WORKFLOW" report freeze-report)"
MIKE_BODY="$(extract "$PAGES_WORKFLOW" mike pages-mike)"

# Each subject must still contain the act the cases assert on. Without this a
# body rewritten to do nothing would satisfy every negative case in the suite.
grep -q 'gh issue create' "$REPORT_BODY" ||
	refuse "the report step no longer runs \`gh issue create\`, so its cases assert about a step that has stopped acting"
grep -q 'git push origin gh-pages' "$MIKE_BODY" ||
	refuse "the mike step no longer runs \`git push origin gh-pages\`, so its cases assert about a step that has stopped acting"

# --- sandboxes --------------------------------------------------------------

# new_sandbox NAME — a throwaway working directory holding the stub PATH, the
# runner temp dir and a fresh $GITHUB_OUTPUT. Echoes the directory.
new_sandbox() {
	local dir="$WORK/sb-$1"
	rm -rf "$dir"
	mkdir -p "$dir/bin" "$dir/runner-temp" "$dir/scripts/release"
	: >"$dir/outputs"
	: >"$dir/calls"
	printf '%s\n' "$dir"
}

# write_gh_stub DIR — a `gh` that records its argv and answers the two reads the
# report step makes. The answers come from the environment so a case configures
# them without a second stub.
write_gh_stub() {
	cat >"$1/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'gh %s\n' "$*" >>"${STUB_CALLS}"
case "$1 ${2:-}" in
"issue list") printf '%s' "${GH_STUB_EXISTING:-}"; [[ -n "${GH_STUB_EXISTING:-}" ]] && echo ;;
"issue view") printf '%s\n' "${GH_STUB_TITLE:-}" ;;
esac
exit 0
STUB
	chmod +x "$1/bin/gh"
}

# write_delegate DIR RC — the check step's delegate,
# scripts/release/check-candidate-covers-main.sh. It prints $DELEGATE_OUT, which
# a case sets, and then leaves by RC: a number to exit with, or `kill` to die on
# SIGKILL. The last is posed for real rather than as `exit 137`, because a
# signalled death is the shape the runner produces and a stub exiting with the
# number only models the arithmetic.
write_delegate() {
	local dir="$1" rc="$2" path="$1/scripts/release/check-candidate-covers-main.sh"
	cat >"$path" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "${DELEGATE_OUT:-}"
STUB
	if [[ "$rc" == kill ]]; then
		printf 'kill -KILL $$\n' >>"$path"
	else
		printf 'exit %s\n' "$rc" >>"$path"
	fi
	chmod +x "$path"
}

# write_mike_stub DIR — records argv plus the two environment variables the
# body exports, and answers `mike list` from MIKE_STUB_VERSIONS.
write_mike_stub() {
	cat >"$1/bin/mike" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'mike %s\n' "$*" >>"${STUB_CALLS}"
# The exclude list is multi-line, and the calls file is read a line at a time,
# so its newlines become commas rather than going through printf %q, whose
# quoting style is a property of the bash the stub happens to run under.
excludes="${MKDOCS_EXCLUDE_DOCS:-}"
printf 'env MKDOCS_EXCLUDE_DOCS=[%s] GAG_DOCS_SOURCE_REF=[%s]\n' \
	"${excludes//$'\n'/,}" "${GAG_DOCS_SOURCE_REF:-}" >>"${STUB_CALLS}"
if [[ "$1" == list ]]; then
	printf '%s\n' ${MIKE_STUB_VERSIONS:-}
fi
exit 0
STUB
	chmod +x "$1/bin/mike"
	cat >"$1/bin/git" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'git %s\n' "$*" >>"${STUB_CALLS}"
exit 0
STUB
	chmod +x "$1/bin/git"
}

# drive DIR BODY [VAR=VALUE...] — run BODY under the Actions shell inside DIR,
# with the stub bin first on PATH. Echoes the step's exit status; the step's own
# stdout and stderr go to DIR/stepout.
drive() {
	local dir="$1" body="$2"
	shift 2
	local rc=0
	(
		cd "$dir"
		export PATH="$dir/bin:$PATH"
		export STUB_CALLS="$dir/calls"
		export RUNNER_TEMP="$dir/runner-temp"
		export GITHUB_OUTPUT="$dir/outputs"
		env "$@" "${ACTIONS_SHELL[@]}" "$body"
	) >"$dir/stepout" 2>&1 || rc=$?
	printf '%s\n' "$rc"
}

# --- assertions -------------------------------------------------------------

expect_rc() {
	local name="$1" want="$2" got="$3" dir="$4"
	die_if_killed "$name" "$got" "$want"
	if [[ "$got" != "$want" ]]; then
		fail "$name" "want rc=$want got rc=$got"
		sed 's/^/    | /' "$dir/stepout" >&2
		return 1
	fi
	return 0
}

expect_call() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/calls" && return 0
	fail "$name" "no stub was asked to: $needle"
	sed 's/^/    | /' "$dir/calls" >&2
	return 1
}

expect_no_call() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/calls" || return 0
	fail "$name" "a stub was asked to, and must not have been: $needle"
	sed 's/^/    | /' "$dir/calls" >&2
	return 1
}

expect_out_file() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/outputs" && return 0
	fail "$name" "\$GITHUB_OUTPUT does not carry: $needle"
	sed 's/^/    | /' "$dir/outputs" >&2
	return 1
}

expect_no_out_file() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/outputs" || return 0
	fail "$name" "\$GITHUB_OUTPUT carries, and must not: $needle"
	sed 's/^/    | /' "$dir/outputs" >&2
	return 1
}

expect_stdout() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/stepout" && return 0
	fail "$name" "the step did not print: $needle"
	sed 's/^/    | /' "$dir/stepout" >&2
	return 1
}

# ============================================================================
# release-freeze-watch.yml `check` — classifying the delegate's exit code
# ============================================================================
#
# 0 is "the candidate still covers main" and 1 is "it does not"; both are
# findings the report step acts on. 2 is "could not measure" and anything above
# it is the delegate crashing rather than exiting, so all of those must fail the
# job LOUDLY and write no `log` output — with none, the report step's `RC` and
# `LOG` are empty and it cannot retire a candidate nothing measured.

# run_check CASE DELEGATE_RC WANT_RC [LOG_TEXT] — echoes the sandbox.
run_check() {
	local case="$1" drc="$2" want="$3" log="${4:-checked}"
	local dir got
	dir="$(new_sandbox "$case")"
	write_delegate "$dir" "$drc"
	got="$(drive "$dir" "$CHECK_BODY" "DELEGATE_OUT=$log")"
	expect_rc "$case" "$want" "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

dir="$(run_check check-covers-main 0 0)"
if [[ -n "$dir" ]]; then
	expect_out_file check-covers-main "$dir" 'rc=0' &&
		expect_out_file check-covers-main "$dir" 'log<<FREEZE_EOF' &&
		pass check-covers-main 'rc=0 reaches the report step'
fi

dir="$(run_check check-candidate-spent 1 0)"
if [[ -n "$dir" ]]; then
	expect_out_file check-candidate-spent "$dir" 'rc=1' &&
		expect_out_file check-candidate-spent "$dir" 'log<<FREEZE_EOF' &&
		pass check-candidate-spent 'rc=1 reaches the report step'
fi

# The four codes that must not reach the report step. 2 and 7 are exits; 126 and
# 137 below are posed for real, since a delegate that cannot be executed or is
# killed produces a shape a stub exiting with the number does not.
crash_case=(check-unmeasurable check-crash-7)
crash_rc=(2 7)
for i in 0 1; do
	name="${crash_case[$i]}"
	drc="${crash_rc[$i]}"
	dir="$(run_check "$name" "$drc" 1)"
	if [[ -n "$dir" ]]; then
		expect_stdout "$name" "$dir" '::error::release-freeze-watch could not measure' &&
			expect_no_out_file "$name" "$dir" 'log<<FREEZE_EOF' &&
			pass "$name" "delegate rc=$drc fails the job and reports nothing"
	fi
done

dir="$(new_sandbox check-not-executable)"
write_delegate "$dir" 0
chmod -x "$dir/scripts/release/check-candidate-covers-main.sh"
# The control this case IS: a delegate that still ran would exit 0, so assert the
# mutation landed before reading the verdict.
if [[ -x "$dir/scripts/release/check-candidate-covers-main.sh" ]]; then
	fail check-not-executable 'the delegate is still executable, so this case poses nothing'
else
	got="$(drive "$dir" "$CHECK_BODY" "DELEGATE_OUT=")"
	expect_rc check-not-executable 1 "$got" "$dir" &&
		expect_stdout check-not-executable "$dir" '::error::release-freeze-watch could not measure' &&
		expect_no_out_file check-not-executable "$dir" 'log<<FREEZE_EOF' &&
		pass check-not-executable 'a non-executable delegate (126) fails the job'
fi

dir="$(new_sandbox check-killed)"
write_delegate "$dir" kill
got="$(drive "$dir" "$CHECK_BODY" "DELEGATE_OUT=")"
expect_rc check-killed 1 "$got" "$dir" &&
	expect_stdout check-killed "$dir" '::error::release-freeze-watch could not measure' &&
	expect_no_out_file check-killed "$dir" 'log<<FREEZE_EOF' &&
	pass check-killed 'a SIGKILLed delegate (137) fails the job'

# --- the regression the row was filed from ----------------------------------
#
# Re-apply the defect that reached review — `-eq 2` where the crash codes are 2
# and above — and require the crash cases to come out the other way. Without
# this the six cases above pass for a suite that cannot tell the classifier
# apart from one that always fails.
sed 's/-ge 2/-eq 2/' "$CHECK_BODY" >"$WORK/freeze-check-eq2.sh"
if cmp -s "$CHECK_BODY" "$WORK/freeze-check-eq2.sh"; then
	fail check-regression-eq2 'the -ge 2 comparison is gone from the check step, so this control mutates nothing'
else
	dir="$(new_sandbox check-regression-eq2)"
	write_delegate "$dir" 7
	got="$(drive "$dir" "$WORK/freeze-check-eq2.sh" "DELEGATE_OUT=v1.6.0-rc.1 no longer covers main")"
	die_if_killed check-regression-eq2 "$got"
	if [[ "$got" == 0 ]] && grep -qF 'log<<FREEZE_EOF' "$dir/outputs"; then
		pass check-regression-eq2 'the -eq 2 defect lets rc=7 through, so these cases can fail'
	else
		fail check-regression-eq2 "the -eq 2 body did not reproduce the defect (rc=$got)"
	fi
fi

# ============================================================================
# release-freeze-watch.yml `report` — the step that opens the issue
# ============================================================================

# run_report CASE RC LOG EXISTING TITLE — echoes the sandbox.
run_report() {
	local case="$1" rc="$2" log="$3" existing="$4" title="$5"
	local dir got
	dir="$(new_sandbox "$case")"
	write_gh_stub "$dir"
	got="$(drive "$dir" "$REPORT_BODY" \
		"GH_TOKEN=stub" "RC=$rc" "LOG=$log" "SHA=deadbeef" \
		"GH_STUB_EXISTING=$existing" "GH_STUB_TITLE=$title")"
	expect_rc "$case" 0 "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

SPENT_LOG='v1.6.0-rc.1 no longer covers main: 3 pull requests merged since the tag'
SPENT_TITLE='Release freeze: v1.6.0-rc.1 no longer covers main'

dir="$(run_report report-clean-no-issue 0 'covers main' '' '')"
if [[ -n "$dir" ]]; then
	expect_no_call report-clean-no-issue "$dir" 'gh issue create' &&
		expect_no_call report-clean-no-issue "$dir" 'gh issue comment' &&
		expect_call report-clean-no-issue "$dir" 'gh label create release-freeze --force' &&
		pass report-clean-no-issue 'a covering candidate opens nothing'
fi

dir="$(run_report report-resolves-open-issue 0 'covers main' 42 'Release freeze: v1.6.0-rc.1 no longer covers main')"
if [[ -n "$dir" ]]; then
	expect_call report-resolves-open-issue "$dir" 'gh issue comment 42 --body Resolved at deadbeef' &&
		expect_call report-resolves-open-issue "$dir" 'gh issue close 42' &&
		expect_no_call report-resolves-open-issue "$dir" 'gh issue create' &&
		pass report-resolves-open-issue 'a recovered candidate closes the open issue'
fi

# THE POSITIVE CONTROL. A spent candidate with no issue open must open one, so a
# change that silenced every exit code — or a report step that stopped acting —
# fails here instead of passing every "must not act" case above.
dir="$(run_report report-opens-issue 1 "$SPENT_LOG" '' '')"
if [[ -n "$dir" ]]; then
	expect_call report-opens-issue "$dir" "gh issue create --label release-freeze --title $SPENT_TITLE" &&
		pass report-opens-issue 'a spent candidate opens an issue naming it'
fi

dir="$(run_report report-already-reported 1 "$SPENT_LOG" 42 "$SPENT_TITLE")"
if [[ -n "$dir" ]]; then
	expect_no_call report-already-reported "$dir" 'gh issue create' &&
		expect_no_call report-already-reported "$dir" 'gh issue comment' &&
		expect_no_call report-already-reported "$dir" 'gh issue close' &&
		expect_stdout report-already-reported "$dir" 'already reported on issue #42' &&
		pass report-already-reported 'the same finding is not re-reported'
fi

dir="$(run_report report-supersedes 1 "$SPENT_LOG" 42 'Release freeze: v1.5.0-rc.2 no longer covers main')"
if [[ -n "$dir" ]]; then
	expect_call report-supersedes "$dir" "gh issue comment 42 --body Superseded: $SPENT_TITLE" &&
		expect_call report-supersedes "$dir" 'gh issue close 42' &&
		expect_call report-supersedes "$dir" "gh issue create --label release-freeze --title $SPENT_TITLE" &&
		pass report-supersedes 'a new candidate supersedes the open issue'
fi

# The second defect the row names: a crash log carries no `-rc.N` string, so the
# title's sed finds nothing. The issue must still open — losing the report is
# worse than losing the candidate's name — and must say so rather than reading
# as a finding about a candidate called nothing.
dir="$(run_report report-log-without-candidate 1 'delegate died before it could name a candidate' '' '')"
if [[ -n "$dir" ]]; then
	expect_call report-log-without-candidate "$dir" \
		'gh issue create --label release-freeze --title Release freeze: the outstanding candidate no longer covers main' &&
		pass report-log-without-candidate 'a log naming no candidate still opens an issue'
fi

# ============================================================================
# check -> report, in sequence
# ============================================================================
#
# The cross-step control. Each step above is driven with its inputs handed to it;
# here `check` produces them. A change that clamps the delegate's exit code — the
# whole class the row is about — leaves RC empty or zero, and no issue is opened.
dir="$(new_sandbox freeze-end-to-end)"
write_delegate "$dir" 1
write_gh_stub "$dir"
got="$(drive "$dir" "$CHECK_BODY" "DELEGATE_OUT=$SPENT_LOG")"
if ! expect_rc freeze-end-to-end 0 "$got" "$dir"; then
	:
else
	# Read `rc` and the heredoc-delimited `log` back out of $GITHUB_OUTPUT the
	# way the runner does, so the second step is fed what the first published.
	step_rc="$(awk -F= '/^rc=/ { print $2 }' "$dir/outputs")"
	die_if_killed freeze-end-to-end "$step_rc"
	step_log="$(awk '/^log<<FREEZE_EOF$/ { inblk = 1; next } /^FREEZE_EOF$/ { inblk = 0 } inblk { print }' "$dir/outputs")"
	if [[ "$step_rc" != 1 ]]; then
		fail freeze-end-to-end "the check step published rc=${step_rc:-<empty>}, not 1"
	elif [[ "$step_log" != *v1.6.0-rc.1* ]]; then
		fail freeze-end-to-end "the check step published a log that does not name the candidate: ${step_log:-<empty>}"
	else
		got="$(drive "$dir" "$REPORT_BODY" \
			"GH_TOKEN=stub" "RC=$step_rc" "LOG=$step_log" "SHA=deadbeef" \
			"GH_STUB_EXISTING=" "GH_STUB_TITLE=")"
		expect_rc freeze-end-to-end 0 "$got" "$dir" &&
			expect_call freeze-end-to-end "$dir" "gh issue create --label release-freeze --title $SPENT_TITLE" &&
			pass freeze-end-to-end 'a spent candidate travels from check to an open issue'
	fi
fi

# ============================================================================
# pages.yml `mike` — the step that pushes the gh-pages version tree
# ============================================================================
#
# The decision under test is step 3's: `stable` and the default root redirect
# move to a release tag only when it is the HIGHEST released version, so a
# backport to an older supported line never demotes the site from a newer minor.
# Nothing else observes that rule — a wrong answer publishes and the site is the
# report.

# run_mike CASE VERSION ALIAS IS_DEFAULT STABLE_TAG VERSIONS — echoes the sandbox.
run_mike() {
	local case="$1" version="$2" alias="$3" is_default="$4" stable_tag="$5" versions="$6"
	local dir got
	dir="$(new_sandbox "$case")"
	write_mike_stub "$dir"
	got="$(drive "$dir" "$MIKE_BODY" \
		"VERSION=$version" "ALIAS=$alias" "IS_DEFAULT=$is_default" \
		"STABLE_TAG=$stable_tag" "TITLE=$version" "CONFIG=mkdocs.yml" \
		"MIKE_STUB_VERSIONS=$versions")"
	expect_rc "$case" 0 "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

dir="$(run_mike pages-dev-push dev '' false false '')"
if [[ -n "$dir" ]]; then
	expect_call pages-dev-push "$dir" 'mike deploy --config-file mkdocs.yml --update-aliases --title dev dev' &&
		expect_no_call pages-dev-push "$dir" 'mike set-default' &&
		expect_call pages-dev-push "$dir" 'git push origin gh-pages' &&
		expect_call pages-dev-push "$dir" 'env MKDOCS_EXCLUDE_DOCS=[/README.md,/releases/,]' &&
		pass pages-dev-push 'the dev version excludes the internal docs and publishes'
fi

# THE POSITIVE CONTROL for this subject: the release that IS the highest must
# claim `stable`, move the default, and push. A body that stopped acting passes
# the backport case below and fails here.
dir="$(run_mike pages-highest-release 1.6.0 '' false true '1.5.1 1.6.0')"
if [[ -n "$dir" ]]; then
	expect_call pages-highest-release "$dir" \
		'mike deploy --config-file mkdocs.yml --alias-type=copy --update-aliases --title 1.6.0 1.6.0 stable' &&
		expect_call pages-highest-release "$dir" 'mike set-default --config-file mkdocs.yml stable' &&
		expect_call pages-highest-release "$dir" 'git push origin gh-pages' &&
		expect_out_file pages-highest-release "$dir" 'claimed_alias=stable' &&
		expect_call pages-highest-release "$dir" 'GAG_DOCS_SOURCE_REF=[v1.6.0]' &&
		pass pages-highest-release 'the highest release claims stable and publishes'
fi

dir="$(run_mike pages-backport 1.5.1 '' false true '1.5.1 1.6.0')"
if [[ -n "$dir" ]]; then
	expect_no_call pages-backport "$dir" 'mike set-default' &&
		expect_no_call pages-backport "$dir" 'stable' &&
		expect_call pages-backport "$dir" 'git push origin gh-pages' &&
		expect_out_file pages-backport "$dir" 'claimed_alias=' &&
		expect_stdout pages-backport "$dir" 'Backport: 1.5.1 is not the highest release (1.6.0)' &&
		pass pages-backport 'a backport publishes its own version and leaves stable alone'
fi

dir="$(run_mike pages-dispatch-alias 1.4.0 v1.4 true false '')"
if [[ -n "$dir" ]]; then
	expect_call pages-dispatch-alias "$dir" \
		'mike deploy --config-file mkdocs.yml --update-aliases --title 1.4.0 --alias-type=copy 1.4.0 v1.4' &&
		expect_call pages-dispatch-alias "$dir" 'mike set-default --config-file mkdocs.yml v1.4' &&
		expect_call pages-dispatch-alias "$dir" 'git push origin gh-pages' &&
		expect_out_file pages-dispatch-alias "$dir" 'claimed_alias=v1.4' &&
		pass pages-dispatch-alias 'a dispatch alias and default are applied verbatim'
fi

# --- the backport rule, deleted ---------------------------------------------
#
# Drop the highest-release comparison so every release tag claims `stable`, and
# require the backport case to go the other way. Without this, pages-backport
# passes for a step that has stopped reaching step 3 at all.
# The ${...} here are the workflow's own text, matched literally.
# shellcheck disable=SC2016
sed 's/if \[\[ "${highest}" == "${VERSION}" \]\]; then/if true; then/' "$MIKE_BODY" >"$WORK/pages-mike-always-stable.sh"
if cmp -s "$MIKE_BODY" "$WORK/pages-mike-always-stable.sh"; then
	fail pages-regression-backport 'the highest-release comparison is gone from the mike step, so this control mutates nothing'
else
	dir="$(new_sandbox pages-regression-backport)"
	write_mike_stub "$dir"
	got="$(drive "$dir" "$WORK/pages-mike-always-stable.sh" \
		"VERSION=1.5.1" "ALIAS=" "IS_DEFAULT=false" "STABLE_TAG=true" \
		"TITLE=1.5.1" "CONFIG=mkdocs.yml" "MIKE_STUB_VERSIONS=1.5.1 1.6.0")"
	die_if_killed pages-regression-backport "$got"
	if [[ "$got" == 0 ]] && grep -qF 'mike set-default --config-file mkdocs.yml stable' "$dir/calls"; then
		pass pages-regression-backport 'without the comparison a backport demotes the site, so this case can fail'
	else
		fail pages-regression-backport "the mutated body did not reproduce the defect (rc=$got)"
	fi
fi

# ============================================================================
# Completeness: every workflow that acts is classified
# ============================================================================
#
# Without this the suite covers a frozen pair, and a `gh issue create` added to
# workflow thirty-one is invisible — the same false negative the path-filter gate
# exists for one rung over. So every workflow carrying an acting command is
# either driven above or listed here with why it is not, and an entry naming a
# workflow that no longer acts fails too: a stale exclusion is how the set drifts
# back.
#
# The scan is line-based over non-comment lines rather than scoped to `run:`
# bodies, which over-reports at worst — a mention in a `with:` block costs one
# registry line and a decision, which is the outcome this assertion wants.
#
# It under-reports in exactly one shape, and the `delegated` entries below are
# it: a body whose whole content is a call into scripts/, where the act happens
# a file away and the decisions are already covered by that script's own suite.
# Those are listed for the reader rather than because the scan found them, so
# they are exempt from the stale-entry check — nothing there can stop matching a
# pattern it never matched.
ACTING_PATTERN='gh (issue|pr|release|label) |git (push|tag)|docker push|helm push|crane |cosign sign|mike (deploy|set-default)'

# workflow|disposition|why
acting_registry() {
	cat <<'EOF'
release-freeze-watch.yml|driven|its check and report steps are the subject above
pages.yml|driven|its mike step is the subject above
dependabot-go-sync.yml|pre-merge|pull_request is its only trigger, so every Dependabot PR executes this body before it merges
e2e-reusable.yml|pre-merge|called by the e2e lanes on merge_group, so the queue runs it on the candidate merge; and its docker push targets the registry the same job stands up
dependabot-rebase-stale.yml|delegated|the body picks one --dry-run flag and calls scripts/ci/dependabot-rebase-stale.sh, whose decisions are covered by ci/dependabot-rebase-stale-test
updatecli.yml|delegated|the body picks apply vs diff and calls the updatecli binary; what it changes is updatecli.d/, not this script
publish.yml|deferred|Q1056 — the release lane's acting steps need cosign, syft, docker, helm and gh stubs, a driver several times this one; its wiring is held statically by ci/check-publish-digest-test and the cosign-pin gate meanwhile
EOF
}

# workflow_acts FILE — true when a non-comment line runs an acting command.
#
# One awk rather than `grep -v | grep -q`: `grep -q` exits on its first match and
# closes the pipe, the upstream grep dies of SIGPIPE, and `pipefail` — which this
# script sets — reports 141 for a pipeline that FOUND what it was looking for.
# Whether the writer has finished by then depends on the file's size and the
# machine's load, so the two-grep form reports a 900-line publish.yml as carrying
# no acting command intermittently. Caught here by a control that failed once and
# then passed in isolation; the same trap is why these bodies are driven under
# `bash -e` without pipefail, above.
workflow_acts() {
	awk -v pat="$ACTING_PATTERN" '
		/^[[:space:]]*#/ { next }
		$0 ~ pat { found = 1; exit }
		END { exit !found }
	' "$1"
}

registry_names="$(acting_registry | cut -d'|' -f1)"
for wf in .github/workflows/*.yml; do
	base="${wf##*/}"
	if workflow_acts "$wf" && ! grep -qxF "$base" <<<"$registry_names"; then
		fail "registry:$base" 'this workflow runs an acting command and is not classified — drive it above, or add a line to acting_registry() saying why not'
	fi
done
while IFS='|' read -r base disposition _; do
	[[ -n "$base" ]] || continue
	if [[ ! -f ".github/workflows/$base" ]]; then
		fail "registry:$base" 'acting_registry() names a workflow that no longer exists'
	elif [[ "$disposition" != delegated ]] && ! workflow_acts ".github/workflows/$base"; then
		fail "registry:$base" 'acting_registry() classifies a workflow that no longer runs an acting command — drop the line'
	fi
done < <(acting_registry)
((fails)) || pass registry-complete 'every workflow that acts is driven or classified'

if ((fails)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall workflow acting-step tests passed\n'
