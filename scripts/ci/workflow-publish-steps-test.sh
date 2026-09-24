#!/usr/bin/env bash
#
# Behavioural tests for publish.yml's acting `run:` bodies (Q1056): the release
# lane's steps that push a chart, sign it, and create, fill and publish the
# GitHub Release — driven with stubs on PATH and asserted on what the stubs were
# asked to do.
#
# WHY THIS WORKFLOW HAS ITS OWN SUITE.
# Q1006 drives the acting bodies of release-freeze-watch.yml and pages.yml in
# workflow-acting-steps-test.sh, and classified publish.yml as deferred there:
# its bodies shell out to helm, cosign, yq, docker and gh, several recording
# multi-line output a later line parses, which is a stub set that does not fit
# beside a `gh issue create` recorder. That is the only reason the two suites are
# separate; the extraction and case shape are the same, and the shell constant
# below is the same claim about GitHub's default `run:` shell.
#
# publish.yml triggers only on a `v*` tag push and a dispatch, so no pull request
# executes any of its bodies: the first run of each is the release it is
# publishing. check-publish-digest.sh, cosign-pin-check and
# semver-floor-sources-check read its YAML, and none of them enters a step's
# shell, so what a body DECIDES is unread before a release. The three decisions
# this suite exists for:
#
#   * a chart push whose digest did not parse must sign nothing, because a
#     signature over an empty ref is a signature over the floating tag;
#   * `gh release create` must not touch a Release a maintainer already curated,
#     and must refuse a digest that is not a sha256;
#   * `gh release edit --draft=false` claims `--latest` from the tag's
#     prerelease state alone.
#
# THE POSITIVE CONTROLS ARE THE LOAD-BEARING HALF, as in the Q1006 suite. A body
# that stopped acting passes every "must not act" case, so each subject carries a
# case that must STILL act, and three `regression` cases re-apply a deleted guard
# to the extracted body and require the assertions to go red.
#
# Runs under `make check` (via `make scripts-test`) and the CI scripts job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
EXTRACT="$REPO_ROOT/scripts/ci/workflow-step-body.sh"

WORKFLOW=".github/workflows/publish.yml"

WORK="$REPO_ROOT/tmp/workflow-publish-steps.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- the verdict ledger -----------------------------------------------------
#
# Every verdict lands in a FILE rather than a shell variable, because most cases
# below run their first assertion inside `dir="$(run_… )"`. A command
# substitution is a subshell, so a `fails=$((fails + 1))` there increments a copy
# the parent never sees: the FAIL line prints to stderr, the case's remaining
# assertions are skipped, and the suite exits 0 reporting that everything passed.
# An append from a subshell reaches the parent, so the ledger counts what the
# variable could not.
#
# REPORTS carries every verdict, pass or fail. Each case reports exactly once, so
# a case that vanished — deleted, or skipped because its `$dir` came back empty —
# shows up as a count one short rather than as one fewer line in a log nobody
# diffs.
FAILURES="$WORK/failures"
REPORTS="$WORK/reports"
: >"$FAILURES"
: >"$REPORTS"
fails=0

# The 19 cases plus registry-complete. Bump it with the case you add.
EXPECTED_REPORTS=20

fail() {
	printf 'FAIL %-38s %s\n' "$1" "$2" >&2
	printf '%s\n' "$1" >>"$FAILURES"
	printf '%s\n' "$1" >>"$REPORTS"
}

pass() {
	printf 'ok   %-38s %s\n' "$1" "$2"
	printf '%s\n' "$1" >>"$REPORTS"
}

# count_fails — read the ledger back into $fails. Called wherever the count is
# about to decide something, never assumed to be current.
count_fails() { fails="$(grep -c . "$FAILURES" || true)"; }

# refuse MESSAGE — exit 2 for anything that would leave this suite driving
# nothing. A body that failed to extract runs clean, and every case built on it
# would report green while asserting about an empty file.
refuse() {
	printf 'workflow-publish-steps: %s\n' "$1" >&2
	exit 2
}

[[ -f "$WORKFLOW" ]] || refuse "$WORKFLOW does not exist, so there are no bodies to drive"

# --- the shell these bodies actually get ------------------------------------
#
# GitHub's default `run:` shell is `bash --noprofile --norc -e {0}`; `-o
# pipefail` arrives only with an explicit `shell: bash`, a job
# `defaults.run.shell` or a workflow-level one. publish.yml sets none, and each
# body opens with its own `set -euo pipefail`, so pipefail is on INSIDE the body
# and off for the shell that runs it — which is what the runner gives it. A
# `shell:` or `defaults:` key appearing here means this constant is stale.
ACTIONS_SHELL=(bash --noprofile --norc -e)

if grep -nE '^[[:space:]]*(shell|defaults):' "$WORKFLOW" >/dev/null; then
	refuse "$WORKFLOW now sets shell: or defaults:, so \`${ACTIONS_SHELL[*]}\` may no longer be the shell its steps get — re-derive it from GitHub's defaults before trusting this suite"
fi

# --- extract the bodies -----------------------------------------------------
#
# Verbatim from the tracked workflow, so a step renamed or rewritten fails here
# rather than leaving the cases below asserting against a stale copy.
extract() {
	local out="$WORK/$2.sh"
	"$EXTRACT" "$WORKFLOW" "$1" >"$out" || refuse "could not extract \"$1\" from $WORKFLOW"
	printf '%s\n' "$out"
}

CHART_STEP="chart"
CHART_CRDS_STEP="chart_crds_v2"
COMPOSE_STEP="Compose and create the GitHub Release (prerelease-aware, Q293)"
CRD_ASSET_STEP="Render, sign, and attach the v2 CRD manifest to the release (keyless, Q276)"
MIGRATE_STEP="Build, sign, and attach the gag-migrate CLI binaries (keyless, Q306)"
PUBLISH_STEP="Publish the release now every asset is attached"

CHART_BODY="$(extract "$CHART_STEP" chart)"
CHART_CRDS_BODY="$(extract "$CHART_CRDS_STEP" chart-crds-v2)"
COMPOSE_BODY="$(extract "$COMPOSE_STEP" compose-release)"
CRD_ASSET_BODY="$(extract "$CRD_ASSET_STEP" crd-asset)"
MIGRATE_BODY="$(extract "$MIGRATE_STEP" migrate-binaries)"
PUBLISH_BODY="$(extract "$PUBLISH_STEP" publish-release)"

# Each subject must still contain the act its cases assert on. Without this a
# body rewritten to do nothing would satisfy every negative case in the suite.
grep -q 'helm push' "$CHART_BODY" ||
	refuse "the chart step no longer runs \`helm push\`, so its cases assert about a step that has stopped acting"
grep -q 'cosign sign' "$CHART_CRDS_BODY" ||
	refuse "the v2 CRD chart step no longer runs \`cosign sign\`, so its cases assert about a step that has stopped acting"
grep -q 'gh release create' "$COMPOSE_BODY" ||
	refuse "the compose step no longer runs \`gh release create\`, so its cases assert about a step that has stopped acting"
grep -q 'gh release upload' "$CRD_ASSET_BODY" ||
	refuse "the v2 CRD asset step no longer runs \`gh release upload\`, so its cases assert about a step that has stopped acting"
grep -q 'gh release upload' "$MIGRATE_BODY" ||
	refuse "the gag-migrate step no longer runs \`gh release upload\`, so its cases assert about a step that has stopped acting"
grep -q 'gh release edit' "$PUBLISH_BODY" ||
	refuse "the publish step no longer runs \`gh release edit\`, so its cases assert about a step that has stopped acting"

# --- sandboxes --------------------------------------------------------------
#
# Every case builds its own, so no state crosses between them: a shared calls
# file would let one case's `cosign sign` satisfy the next case's assertion that
# one happened, and the "signs nothing" cases are exactly the ones that would
# then pass for the wrong reason.

# new_sandbox NAME — a throwaway working directory holding the stub PATH, the
# runner temp dir, a fresh $GITHUB_OUTPUT, and the in-repo helpers the bodies
# invoke by relative path. Echoes the directory.
new_sandbox() {
	local dir="$WORK/sb-$1"
	rm -rf "$dir"
	mkdir -p "$dir/bin" "$dir/runner-temp" "$dir/scripts/fetch" "$dir/scripts/release" \
		"$dir/charts/actions-gateway" "$dir/charts/actions-gateway-crds-v2"
	: >"$dir/outputs"
	: >"$dir/calls"
	# The REAL retry wrapper, not a stub: the bodies route every registry call
	# through it, so driving a copy keeps its "retry then give up" behaviour in
	# the path under test. RETRY_ATTEMPTS=1 in drive() keeps a failure case from
	# sleeping through four backoffs.
	cp "$REPO_ROOT/scripts/fetch/retry.sh" "$dir/scripts/fetch/retry.sh"
	chmod +x "$dir/scripts/fetch/retry.sh"
	printf '%s\n' "$dir"
}

# write_release_stubs DIR — the five binaries the release bodies shell out to,
# each recording its argv to $STUB_CALLS before anything else, so a case that
# asserts a call did NOT happen reads the same record as one that asserts it
# did. What a stub answers comes from the environment, so a case configures it
# without a second stub.
#
# `helm push` and `docker buildx imagetools inspect` are the two that matter:
# each prints output a later line of the body parses, and both parses are a
# decision this suite is here to read. The rest record and succeed.
write_release_stubs() {
	local dir="$1"
	cat >"$dir/bin/helm" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'helm %s\n' "$*" >>"${STUB_CALLS}"
case "$1" in
push)
	# helm prints "Digest: sha256:…" on its own line among others, and the body
	# awks for it. An empty HELM_STUB_DIGEST models a push whose output carried
	# no digest line at all.
	printf 'Pushed: ghcr.io/example/charts/x\n'
	if [[ -n "${HELM_STUB_DIGEST:-}" ]]; then printf 'Digest: %s\n' "${HELM_STUB_DIGEST}"; fi
	;;
template) printf '%s\n' "${HELM_STUB_TEMPLATE:-rendered crds}" ;;
esac
exit 0
STUB
	cat >"$dir/bin/cosign" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'cosign %s\n' "$*" >>"${STUB_CALLS}"
exit 0
STUB
	cat >"$dir/bin/yq" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'yq %s\n' "$*" >>"${STUB_CALLS}"
exit 0
STUB
	cat >"$dir/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'docker %s\n' "$*" >>"${STUB_CALLS}"
# `buildx imagetools inspect --format '{{.Manifest.Digest}}'` is the index-digest
# resolver the release notes pin to. DOCKER_STUB_DIGEST is what it answers.
if [[ "${1:-}" == buildx ]]; then printf '%s\n' "${DOCKER_STUB_DIGEST:-}"; fi
exit 0
STUB
	cat >"$dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'gh %s\n' "$*" >>"${STUB_CALLS}"
# The one read the bodies make: whether the tag already has a Release. gh exits
# non-zero when it does not, which is what the guards branch on.
if [[ "$1 ${2:-}" == "release view" ]]; then
	[[ "${GH_STUB_RELEASE_EXISTS:-false}" == true ]] || exit 1
fi
exit 0
STUB
	chmod +x "$dir/bin/helm" "$dir/bin/cosign" "$dir/bin/yq" "$dir/bin/docker" "$dir/bin/gh"
}

# write_migrate_delegate DIR — scripts/release/build-migrate-binaries.sh, which
# the gag-migrate step calls and then globs the output of. It creates whatever
# $MIGRATE_STUB_BINARIES names; empty models a build that produced none, which
# is the case the unmatched glob turns on.
write_migrate_delegate() {
	local path="$1/scripts/release/build-migrate-binaries.sh"
	cat >"$path" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf 'build-migrate-binaries %s\n' "$*" >>"${STUB_CALLS}"
mkdir -p "$1"
# Deliberately unquoted: the variable is a list of names, not one name.
for f in ${MIGRATE_STUB_BINARIES:-}; do : >"$1/$f"; done
: >"$1/SHA256SUMS"
exit 0
STUB
	chmod +x "$path"
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
		export RETRY_ATTEMPTS=1
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

expect_stdout() {
	local name="$1" dir="$2" needle="$3"
	grep -qF -- "$needle" "$dir/stepout" && return 0
	fail "$name" "the step did not print: $needle"
	sed 's/^/    | /' "$dir/stepout" >&2
	return 1
}

expect_file_has() {
	local name="$1" file="$2" needle="$3"
	[[ -f "$file" ]] || { fail "$name" "$file was never written"; return 1; }
	grep -qF -- "$needle" "$file" && return 0
	fail "$name" "$file does not carry: $needle"
	sed 's/^/    | /' "$file" >&2
	return 1
}

CHART_DIGEST='sha256:1111111111111111111111111111111111111111111111111111111111111111'
INDEX_DIGEST='sha256:2222222222222222222222222222222222222222222222222222222222222222'

# ============================================================================
# `chart` / `chart_crds_v2` — the digest a signature binds to
# ============================================================================
#
# `helm push` prints the pushed bytes' digest and the body awks it back out, so
# the signature binds to those bytes rather than to the floating tag a
# re-push would move. The decision is what happens when that parse comes back
# empty: signing `…/charts/actions-gateway@` with no digest is a signature over
# the tag, which verifies today and stops matching the moment anything re-pushes.

# run_chart CASE BODY DIGEST — echoes the sandbox.
run_chart() {
	local case="$1" body="$2" digest="$3" want="${4:-0}"
	local dir got
	dir="$(new_sandbox "$case")"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$body" \
		"TAG=v1.6.0" "OWNER=actions-gateway" "PRERELEASE=false" "REGISTRY=ghcr.io" \
		"HELM_STUB_DIGEST=$digest")"
	expect_rc "$case" "$want" "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

# THE POSITIVE CONTROL for this subject: the push that parsed must sign the
# digest-pinned ref and publish it for the summary step.
dir="$(run_chart chart-signs-pushed-digest "$CHART_BODY" "$CHART_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_call chart-signs-pushed-digest "$dir" \
		"cosign sign --yes ghcr.io/actions-gateway/charts/actions-gateway@$CHART_DIGEST" &&
		expect_call chart-signs-pushed-digest "$dir" 'helm package charts/actions-gateway' &&
		expect_out_file chart-signs-pushed-digest "$dir" \
			"ref=ghcr.io/actions-gateway/charts/actions-gateway@$CHART_DIGEST" &&
		pass chart-signs-pushed-digest 'the signature binds to the digest helm push reported'
fi

dir="$(run_chart chart-unparsed-digest-signs-nothing "$CHART_BODY" '' 1)"
if [[ -n "$dir" ]]; then
	expect_no_call chart-unparsed-digest-signs-nothing "$dir" 'cosign sign' &&
		expect_stdout chart-unparsed-digest-signs-nothing "$dir" \
			'could not parse pushed chart digest' &&
		pass chart-unparsed-digest-signs-nothing 'an unparsed digest fails the step before it signs'
fi

dir="$(run_chart chart-crds-signs-pushed-digest "$CHART_CRDS_BODY" "$CHART_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_call chart-crds-signs-pushed-digest "$dir" \
		"cosign sign --yes ghcr.io/actions-gateway/charts/actions-gateway-crds-v2@$CHART_DIGEST" &&
		pass chart-crds-signs-pushed-digest 'the v2 CRD chart signs its own pushed digest'
fi

dir="$(run_chart chart-crds-unparsed-signs-nothing "$CHART_CRDS_BODY" '' 1)"
if [[ -n "$dir" ]]; then
	expect_no_call chart-crds-unparsed-signs-nothing "$dir" 'cosign sign' &&
		expect_stdout chart-crds-unparsed-signs-nothing "$dir" \
			'could not parse pushed v2 CRD chart digest' &&
		pass chart-crds-unparsed-signs-nothing 'the v2 CRD chart also signs nothing on an unparsed digest'
fi

# --- the guard, deleted -----------------------------------------------------
#
# Drop the empty-digest refusal and require the unparsed case to go the other
# way. Without this, chart-unparsed-digest-signs-nothing passes for a body that
# has stopped pushing at all.
# The ${digest} here is the workflow's own text, matched literally.
# shellcheck disable=SC2016
awk '/^if \[\[ -z "\$\{digest\}" \]\]; then$/ { skip = 4 } skip { skip--; next } { print }' \
	"$CHART_BODY" >"$WORK/chart-no-guard.sh"
# The awk swallows the four-line guard block; reconcile that against the
# original rather than trusting the line count.
if cmp -s "$CHART_BODY" "$WORK/chart-no-guard.sh" ||
	grep -q 'could not parse pushed chart digest' "$WORK/chart-no-guard.sh"; then
	fail chart-regression-no-guard 'the empty-digest refusal did not come out of the body, so this control mutates nothing'
else
	dir="$(new_sandbox chart-regression-no-guard)"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$WORK/chart-no-guard.sh" \
		"TAG=v1.6.0" "OWNER=actions-gateway" "PRERELEASE=false" "REGISTRY=ghcr.io" \
		"HELM_STUB_DIGEST=")"
	die_if_killed chart-regression-no-guard "$got"
	if [[ "$got" == 0 ]] && grep -qF 'cosign sign --yes ghcr.io/actions-gateway/charts/actions-gateway@' "$dir/calls"; then
		pass chart-regression-no-guard 'without the refusal an empty digest is signed, so these cases can fail'
	else
		fail chart-regression-no-guard "the unguarded body did not reproduce the defect (rc=$got)"
	fi
fi

# ============================================================================
# `Compose and create the GitHub Release` — the notes and the create guard
# ============================================================================
#
# Two decisions. The Release is created ONLY when the tag has none, so a
# maintainer who curated notes ahead of the tag is never clobbered; and every
# image digest in the notes is resolved from the just-pushed tag and must be a
# sha256, because those are the refs operators pin to.

# run_compose CASE PRERELEASE EXISTS DIGEST WANT_RC — echoes the sandbox.
run_compose() {
	local case="$1" prerelease="$2" exists="$3" digest="$4" want="${5:-0}"
	local dir got
	dir="$(new_sandbox "$case")"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$COMPOSE_BODY" \
		"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" \
		"OWNER=actions-gateway" "PRERELEASE=$prerelease" "REGISTRY=ghcr.io" \
		"GH_TOKEN=stub" "GH_STUB_RELEASE_EXISTS=$exists" "DOCKER_STUB_DIGEST=$digest")"
	expect_rc "$case" "$want" "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

# THE POSITIVE CONTROL for this subject.
dir="$(run_compose compose-creates-draft false false "$INDEX_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_call compose-creates-draft "$dir" \
		'gh release create v1.6.0 --repo actions-gateway/github-actions-gateway --title v1.6.0 --draft' &&
		expect_file_has compose-creates-draft "$dir/runner-temp/release-notes.md" \
			"**gmc** — \`ghcr.io/actions-gateway/gmc@$INDEX_DIGEST\`" &&
		expect_file_has compose-creates-draft "$dir/runner-temp/release-notes.md" \
			'make verify-release VERSION=v1.6.0' &&
		pass compose-creates-draft 'a stable tag with no Release gets a draft carrying the six index digests'
fi

dir="$(run_compose compose-stable-not-prerelease false false "$INDEX_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_no_call compose-stable-not-prerelease "$dir" '--prerelease' &&
		pass compose-stable-not-prerelease 'a stable tag is created without --prerelease'
fi

dir="$(run_compose compose-rc-prerelease true false "$INDEX_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_call compose-rc-prerelease "$dir" '--prerelease' &&
		pass compose-rc-prerelease 'a prerelease tag carries --prerelease'
fi

dir="$(run_compose compose-existing-untouched false true "$INDEX_DIGEST")"
if [[ -n "$dir" ]]; then
	expect_no_call compose-existing-untouched "$dir" 'gh release create' &&
		expect_stdout compose-existing-untouched "$dir" \
			'leaving its notes and flags untouched' &&
		pass compose-existing-untouched 'an existing Release is left alone, curated notes intact'
fi

# A resolver that answered something other than a digest — an error string on
# stdout, or nothing at all — must stop the step rather than write a Release
# whose pin lines name a ref operators cannot pull.
dir="$(run_compose compose-bad-digest-refuses false false 'not-a-digest' 1)"
if [[ -n "$dir" ]]; then
	expect_no_call compose-bad-digest-refuses "$dir" 'gh release create' &&
		expect_stdout compose-bad-digest-refuses "$dir" 'could not resolve index digest' &&
		pass compose-bad-digest-refuses 'a non-sha256 digest fails before any Release is created'
fi

# --- the create guard, deleted ----------------------------------------------
#
# Make the existence check always report absent and require the existing-Release
# case to go the other way, so compose-existing-untouched cannot pass for a step
# that has stopped reaching `gh release create` at all.
# The ${TAG}/${REPO} here are the workflow's own text, matched literally.
# shellcheck disable=SC2016
awk '{ sub(/^if gh release view "\$\{TAG\}" --repo "\$\{REPO\}" >\/dev\/null 2>&1; then$/, "if false; then"); print }' \
	"$COMPOSE_BODY" >"$WORK/compose-no-guard.sh"
if cmp -s "$COMPOSE_BODY" "$WORK/compose-no-guard.sh"; then
	fail compose-regression-no-guard 'the existing-Release guard is gone from the compose step, so this control mutates nothing'
else
	dir="$(new_sandbox compose-regression-no-guard)"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$WORK/compose-no-guard.sh" \
		"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" \
		"OWNER=actions-gateway" "PRERELEASE=false" "REGISTRY=ghcr.io" \
		"GH_TOKEN=stub" "GH_STUB_RELEASE_EXISTS=true" "DOCKER_STUB_DIGEST=$INDEX_DIGEST")"
	die_if_killed compose-regression-no-guard "$got"
	if [[ "$got" == 0 ]] && grep -qF 'gh release create v1.6.0' "$dir/calls"; then
		pass compose-regression-no-guard 'without the guard an existing Release is recreated, so that case can fail'
	else
		fail compose-regression-no-guard "the unguarded body did not reproduce the defect (rc=$got)"
	fi
fi

# ============================================================================
# The two asset steps — what gets signed, and what gets attached
# ============================================================================
#
# Both sign a blob keyless and upload with --clobber so a re-publish is
# idempotent. The v2 CRD step also carries a create-if-missing fallback, which is
# the one path in the lane that can produce a Release with no curated notes.

dir="$(new_sandbox crd-asset-uploads)"
write_release_stubs "$dir"
got="$(drive "$dir" "$CRD_ASSET_BODY" \
	"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" "GH_TOKEN=stub" \
	"GH_STUB_RELEASE_EXISTS=true")"
if expect_rc crd-asset-uploads 0 "$got" "$dir"; then
	expect_call crd-asset-uploads "$dir" 'cosign sign-blob --yes --bundle' &&
		expect_call crd-asset-uploads "$dir" 'gh release upload v1.6.0 --repo actions-gateway/github-actions-gateway --clobber' &&
		expect_no_call crd-asset-uploads "$dir" 'gh release create' &&
		expect_file_has crd-asset-uploads "$dir/runner-temp/actions-gateway-crds-v2.yaml" 'rendered crds' &&
		pass crd-asset-uploads 'the rendered CRD manifest is signed and attached to the existing draft'
fi

dir="$(new_sandbox crd-asset-fallback-create)"
write_release_stubs "$dir"
got="$(drive "$dir" "$CRD_ASSET_BODY" \
	"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" "GH_TOKEN=stub" \
	"GH_STUB_RELEASE_EXISTS=false")"
if expect_rc crd-asset-fallback-create 0 "$got" "$dir"; then
	expect_call crd-asset-fallback-create "$dir" \
		'gh release create v1.6.0 --repo actions-gateway/github-actions-gateway --title v1.6.0 --draft --generate-notes' &&
		expect_call crd-asset-fallback-create "$dir" 'gh release upload v1.6.0' &&
		pass crd-asset-fallback-create 'a missing Release is created as a draft so the upload cannot fail'
fi

dir="$(new_sandbox migrate-uploads-binaries)"
write_release_stubs "$dir"
write_migrate_delegate "$dir"
got="$(drive "$dir" "$MIGRATE_BODY" \
	"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" "GH_TOKEN=stub" \
	"MIGRATE_STUB_BINARIES=gag-migrate-linux-amd64 gag-migrate-darwin-arm64")"
if expect_rc migrate-uploads-binaries 0 "$got" "$dir"; then
	expect_call migrate-uploads-binaries "$dir" 'cosign sign-blob --yes --bundle' &&
		expect_call migrate-uploads-binaries "$dir" 'gag-migrate-linux-amd64' &&
		expect_call migrate-uploads-binaries "$dir" 'gag-migrate-darwin-arm64' &&
		expect_call migrate-uploads-binaries "$dir" 'SHA256SUMS.cosign.bundle' &&
		pass migrate-uploads-binaries 'every built binary, the manifest and its bundle are attached'
fi

# A build that produced no binaries. Bash leaves an unmatched glob as its own
# literal text, so `gh` is handed a path containing `*` rather than the step
# stopping: the release publishes with a signed SHA256SUMS over nothing and no
# CLI assets, and whether the run fails at all is gh's call, not the body's.
dir="$(new_sandbox migrate-no-binaries)"
write_release_stubs "$dir"
write_migrate_delegate "$dir"
got="$(drive "$dir" "$MIGRATE_BODY" \
	"TAG=v1.6.0" "REPO=actions-gateway/github-actions-gateway" "GH_TOKEN=stub" \
	"MIGRATE_STUB_BINARIES=")"
if expect_rc migrate-no-binaries 0 "$got" "$dir"; then
	expect_call migrate-no-binaries "$dir" '/migrate-bin/gag-migrate-*' &&
		pass migrate-no-binaries 'a build producing no binaries reaches gh with the glob unexpanded'
fi

# ============================================================================
# `Publish the release now every asset is attached` — the --latest decision
# ============================================================================
#
# The last step to touch the Release, because publishing seals an immutable one.
# `--latest` is explicit rather than inherited, and it is decided by the tag's
# prerelease state ALONE: the step makes no comparison against the versions
# already released. A stable backport cut after a newer minor therefore claims
# `latest` and demotes that minor — the asymmetry with pages.yml, whose `mike`
# step moves `stable` only to the highest released version. The cases below
# record what the step does; whether a backport should claim it is a question
# about the release design rather than about this body, and it is filed.

# run_publish CASE TAG PRERELEASE — echoes the sandbox.
run_publish() {
	local case="$1" tag="$2" prerelease="$3"
	local dir got
	dir="$(new_sandbox "$case")"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$PUBLISH_BODY" \
		"TAG=$tag" "REPO=actions-gateway/github-actions-gateway" \
		"PRERELEASE=$prerelease" "GH_TOKEN=stub")"
	expect_rc "$case" 0 "$got" "$dir" || return 0
	printf '%s\n' "$dir"
}

# THE POSITIVE CONTROL for this subject: the draft must actually be flipped.
dir="$(run_publish publish-stable-claims-latest v1.6.0 false)"
if [[ -n "$dir" ]]; then
	expect_call publish-stable-claims-latest "$dir" \
		'gh release edit v1.6.0 --repo actions-gateway/github-actions-gateway --draft=false --latest' &&
		pass publish-stable-claims-latest 'a stable tag is published and claims latest'
fi

dir="$(run_publish publish-rc-keeps-latest v1.6.0-rc.1 true)"
if [[ -n "$dir" ]]; then
	expect_call publish-rc-keeps-latest "$dir" \
		'gh release edit v1.6.0-rc.1 --repo actions-gateway/github-actions-gateway --draft=false' &&
		expect_no_call publish-rc-keeps-latest "$dir" '--latest' &&
		pass publish-rc-keeps-latest 'a prerelease is published without claiming latest'
fi

dir="$(run_publish publish-backport-claims-latest v1.5.1 false)"
if [[ -n "$dir" ]]; then
	expect_call publish-backport-claims-latest "$dir" \
		'gh release edit v1.5.1 --repo actions-gateway/github-actions-gateway --draft=false --latest' &&
		pass publish-backport-claims-latest 'a backport claims latest too — the flag reads the tag, not the released set'
fi

# --- the prerelease test, inverted ------------------------------------------
#
# Flip the comparison so the flag follows the opposite state, and require the
# prerelease case to go the other way. Without this, publish-rc-keeps-latest
# passes for a body that has stopped passing --latest at all.
# The ${PRERELEASE} here is the workflow's own text, matched literally.
# shellcheck disable=SC2016
awk '{ sub(/if \[\[ "\$\{PRERELEASE\}" != "true" \]\]; then/, "if [[ \"${PRERELEASE}\" == \"true\" ]]; then"); print }' \
	"$PUBLISH_BODY" >"$WORK/publish-inverted.sh"
if cmp -s "$PUBLISH_BODY" "$WORK/publish-inverted.sh"; then
	fail publish-regression-inverted 'the prerelease comparison is gone from the publish step, so this control mutates nothing'
else
	dir="$(new_sandbox publish-regression-inverted)"
	write_release_stubs "$dir"
	got="$(drive "$dir" "$WORK/publish-inverted.sh" \
		"TAG=v1.6.0-rc.1" "REPO=actions-gateway/github-actions-gateway" \
		"PRERELEASE=true" "GH_TOKEN=stub")"
	die_if_killed publish-regression-inverted "$got"
	if [[ "$got" == 0 ]] && grep -qF -- '--draft=false --latest' "$dir/calls"; then
		pass publish-regression-inverted 'inverted, an RC claims latest, so that case can fail'
	else
		fail publish-regression-inverted "the inverted body did not reproduce the defect (rc=$got)"
	fi
fi

# ============================================================================
# Completeness: every acting step in publish.yml is driven or classified
# ============================================================================
#
# One rung below the Q1006 suite's whole-workflow registry, which holds
# publish.yml to being covered somewhere. This holds each of its acting STEPS,
# so a seventh added to the release lane cannot publish uncovered.
#
# The scan is line-based within each step, over the same pattern the Q1006 suite
# uses one rung up, and over-reports at worst: a spurious match costs one
# registry line and a decision, which is the outcome this assertion wants.
#
# It skips two line shapes rather than matching them, because at step level a
# spurious match is not free — an exclusion written for a prose mention would
# also cover a real act appearing later in the same step. A comment is the
# obvious one. The other is a line whose command is `echo` or `printf`: those
# emit text and cannot act, and publish.yml's announce-bar gate would otherwise
# read as acting on the strength of an `::error::` message that says "the git
# tags".
#
# Anchoring the pattern to command position instead would be the stronger rule,
# and it is the wrong one here: every registry call in this lane is wrapped,
# `scripts/fetch/retry.sh cosign sign …`, so `cosign sign` is never in command
# position and the whole lane would scan as inert.
ACTING_PATTERN='gh (issue|pr|release|label) |git (push|tag)|docker push|helm push|crane |cosign sign|mike (deploy|set-default)'

# EMITS — an awk prelude, shared by the scan and the read-only check below so
# the two cannot disagree about which lines are able to act.
EMITS='function emits(s) {
	sub(/^[ \t]+/, "", s)
	return (s ~ /^#/ || s ~ /^echo[ \t]/ || s ~ /^printf[ \t]/) }'

# step|disposition|why
step_registry() {
	cat <<'EOF'
chart|driven|packages, pushes and signs the main chart
chart_crds_v2|driven|packages, pushes and signs the opt-in v2 CRD chart
Compose and create the GitHub Release (prerelease-aware, Q293)|driven|creates the draft Release and writes its notes
Render, sign, and attach the v2 CRD manifest to the release (keyless, Q276)|driven|signs and attaches the rendered CRD manifest
Build, sign, and attach the gag-migrate CLI binaries (keyless, Q306)|driven|signs and attaches the CLI binaries
Publish the release now every asset is attached|driven|flips the draft and decides --latest
Sign image + attest SBOMs (keyless)|undrivable|interpolates a ${{ }} expression into its script, so a driven body would not be the body CI runs; its wiring is held by ci/check-publish-digest-test and the cosign-pin gate
The rendered announce bar names this release|read-only|it runs `git tag --list` to find the highest release, and the scan's `git tag` pattern cannot tell a list from a create
EOF
}

# A `read-only` entry is checked rather than believed: every line of the step's
# body that matched the acting pattern must be one of these reads. A second,
# genuinely acting line makes the entry stale, and a stale exclusion is how the
# covered set drifts back.
READ_ONLY_FORMS='git tag --list'

# publish_acting_steps — emit the selector of each step whose `run:` body runs an
# acting command: its `id:` when it has one, its `name:` otherwise, matching how
# workflow-step-body.sh selects. One awk rather than a grep pipeline: `grep -q`
# closes the pipe on its first match, the upstream stage dies of SIGPIPE, and
# pipefail — which this script sets — reports 141 for a scan that FOUND what it
# was looking for.
publish_acting_steps() {
	awk -v pat="$ACTING_PATTERN" "$EMITS"'
		function flush() {
			if (label != "" && acts) print (id != "" ? id : label)
			label = ""; id = ""; acts = 0
		}
		/^      - / {
			flush()
			if ($0 ~ /^      - name: /) { label = substr($0, 15) }
			else { label = "(uses)" }
			next
		}
		/^        id: / { id = substr($0, 13); next }
		!emits($0) && $0 ~ pat { acts = 1 }
		END { flush() }
	' "$WORKFLOW"
}

count_fails
registry_fails_before=$fails
registry_names="$(step_registry | cut -d'|' -f1)"
while IFS= read -r selector; do
	[[ -n "$selector" ]] || continue
	grep -qxF "$selector" <<<"$registry_names" ||
		fail "registry:$selector" 'this step runs an acting command and is not classified — drive it above, or add a line to step_registry() saying why not'
done < <(publish_acting_steps)

acting_selectors="$(publish_acting_steps)"
while IFS='|' read -r selector disposition _; do
	[[ -n "$selector" ]] || continue
	if ! grep -qxF "$selector" <<<"$acting_selectors"; then
		fail "registry:$selector" 'step_registry() classifies a step that no longer exists or no longer runs an acting command — drop the line'
	elif [[ "$disposition" == undrivable ]]; then
		# A live check rather than a comment: the entry claims the extractor
		# refuses this step, so ask it. A step that became drivable must be
		# driven, not left excused by a stale line.
		if "$EXTRACT" "$WORKFLOW" "$selector" >/dev/null 2>&1; then
			fail "registry:$selector" 'step_registry() calls this undrivable, but workflow-step-body.sh extracts it — drive it above'
		fi
	elif [[ "$disposition" == read-only ]]; then
		body="$WORK/read-only.sh"
		"$EXTRACT" "$WORKFLOW" "$selector" >"$body" ||
			refuse "could not extract the read-only step \"$selector\" to check its acting lines"
		if awk -v pat="$ACTING_PATTERN" -v ok="$READ_ONLY_FORMS" "$EMITS"'
			!emits($0) && $0 ~ pat && index($0, ok) == 0 { found = 1; exit }
			END { exit !found }
		' "$body"; then
			fail "registry:$selector" "step_registry() calls this read-only, but a line of its body matches the acting pattern through something other than \`$READ_ONLY_FORMS\` — drive it above"
		fi
	fi
done < <(step_registry)
count_fails
if ((fails == registry_fails_before)); then
	pass registry-complete 'every acting step in publish.yml is driven or classified'
else
	# Reported rather than silent. Keying this line on the GLOBAL count would
	# skip it whenever any earlier case failed, and a reader of a red run could
	# not then tell "the registry assertion passed" from "it never reported".
	fail registry-complete 'the step registry is out of step with the workflow, see the registry: lines above'
fi

# A case that vanished reports neither ok nor FAIL, so the log shrinks by one
# line and nothing else notices. Checked only on an otherwise-green run: a red
# one already exits non-zero, and its count is not a fixed number.
count_fails
if ((fails == 0)); then
	reports="$(grep -c . "$REPORTS" || true)"
	if ((reports != EXPECTED_REPORTS)); then
		fail report-count "the suite reported $reports verdicts, expected $EXPECTED_REPORTS — a case was added or lost without updating EXPECTED_REPORTS"
		count_fails
	fi
fi

if ((fails)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall publish.yml acting-step tests passed\n'
