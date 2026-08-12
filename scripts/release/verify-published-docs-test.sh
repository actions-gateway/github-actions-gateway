#!/usr/bin/env bash
#
# Assert what `make verify-published-docs` catches and what it deliberately does
# not (Q784).
#
# The gate reads a live site, so the suite runs the real script end to end behind
# a stub `curl` that serves fixture pages — the same shape verify-release-test.sh
# uses for cosign, and for the same reason: asserting the extraction alone leaves
# the script's own paths unexecuted (Q605).
#
# The fixtures reproduce the incident the gate exists for. /1.4.0/ published
# `--version 1.3.0` and the v1.3.0 CRD manifest URL, for three hours, alongside a
# `stable` alias and root redirect that served the same page. That is the
# positive control here: the live /1.4.0/ was repaired by hand, so it no longer
# demonstrates the failure, while /1.1.0/ and /1.2.0/ still do.
#
# The negative controls matter as much. The announce bar names the NEWEST release
# rather than the version being read (hooks/release_version.py), so a correct
# 1.1.0 page announces v1.2.0 in its chrome; a gate that scanned the whole
# document would fail every page it checked. So would `v2.0.0`, the announced
# v1alpha1/v2alpha1 removal release, and the `Measured on kind` line that records
# what an upgrade rehearsal actually installed.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

SCRIPT="$REPO_ROOT/scripts/release/verify-published-docs.sh"
FIXTURE_DIR="$REPO_ROOT/tmp/verify-published-docs-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# Stub curl: resolve the requested URL under $STUB_SITE_ROOT and copy the fixture
# to the -o destination. A path with no fixture is a 404, i.e. a non-zero exit.
cat > "$FIXTURE_DIR/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
dest=""
url=""
while (($# > 0)); do
	case "$1" in
	-o) dest="$2"; shift ;;
	--max-time | --retry | --retry-delay) shift ;;
	-*) ;;
	*) url="$1" ;;
	esac
	shift
done
path="${url#http://site}"
path="${path%/}"
src="$STUB_SITE_ROOT${path}/index.html"
[[ -f "$src" ]] || exit 22
# Stretches a run out so the concurrency case below actually overlaps.
[[ "${STUB_CURL_DELAY:-0}" != 0 ]] && sleep "$STUB_CURL_DELAY"
cp "$src" "$dest"
STUB
chmod +x "$FIXTURE_DIR/curl"

# page PATH ARTICLE_HTML — write a fixture page. The chrome around <article> is
# the part the scan must ignore: the announce bar carries a release version of
# its own, exactly as the built site does.
page() {
	local path="$1" article="$2"
	local dir="$STUB_SITE_ROOT/$path"
	mkdir -p "$dir"
	cat > "$dir/index.html" << HTML
<!doctype html><html><head>
<link rel="canonical" href="http://site/${CANONICAL:-$path}/">
</head><body>
<aside class="md-banner"><p>v9.9.9 is here. Day-2 observability across the v2 API.</p></aside>
<nav class="md-nav"><ul><li><a href="../">Version 3.2.1</a></li></ul></nav>
<div class="md-content"><article class="md-content__inner md-typeset">
$article
</article></div>
<footer><p>Built from v9.9.9</p></footer>
</body></html>
HTML
}

# site NAME — start a fresh fixture site and export its root.
site() {
	STUB_SITE_ROOT="$FIXTURE_DIR/$1"
	rm -rf "$STUB_SITE_ROOT"
	mkdir -p "$STUB_SITE_ROOT"
}

# A version dir whose four pages all pin $1, plus the exempt shapes that must not
# be reported: v2.0.0, and the measurement line that records an older install.
good_version_dir() {
	local v="$1" minor="${1%.*}"
	CANONICAL="$v" page "$v" \
		"<pre><code>helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway <span class=\"w\"> </span>--version <span class=\"m\">${v%%.*}</span>.${v#*.}</code></pre>
		<p>The v1alpha1 API is removed in <code>v2.0.0</code>.</p>"
	CANONICAL="$v" page "$v/operations/install" \
		"<pre><code>--version $v</code></pre>
		<p>Newer patch releases publish as <code>$minor.z</code>; pin the version you have verified.</p>
		<p>CRDs: https://github.com/actions-gateway/github-actions-gateway/releases/download/v$v/actions-gateway-crds-v2.yaml</p>
		<p>Removed in v2.0.0.</p>"
	CANONICAL="$v" page "$v/operations/upgrade" \
		"<pre><code>--version $v</code></pre>
		<p>Measured on kind v1.36.1: install <code>v1.2.0</code>, upgrade to this chart, <code>helm rollback</code> to revision 1.</p>"
	CANONICAL="$v" page "$v/operations/gitops" \
		"<p>targetRevision: <code>$v</code></p><p>tag: <code>v$v</code></p>"
}

# The alias and the root a visitor lands on. CANONICAL names the version dir
# /stable/ actually serves, which is the assertion.
alias_pages() {
	local serves="$1" root_target="${2:-stable}"
	CANONICAL="$serves" page "stable" "<p>--version $serves</p>"
	mkdir -p "$STUB_SITE_ROOT"
	printf '<!doctype html><html><head><meta http-equiv="refresh" content="0; url=%s/">\n</head></html>\n' \
		"$root_target" > "$STUB_SITE_ROOT/index.html"
}

out_text=""
out_rc=0

run() {
	out_rc=0
	out_text="$(CURL="$FIXTURE_DIR/curl" STUB_SITE_ROOT="$STUB_SITE_ROOT" \
		"$SCRIPT" --base http://site "$@" 2>&1)" || out_rc=$?
}

expect_rc() {
	local name="$1" want="$2"
	if [[ "$out_rc" == "$want" ]]; then
		pass "$name (rc=$out_rc)"
	else
		fail "$name" "want rc=$want got rc=$out_rc; output:"$'\n'"$out_text"
	fi
}

expect_says() {
	local name="$1" needle="$2"
	if [[ "$out_text" == *"$needle"* ]]; then
		pass "$name"
	else
		fail "$name" "output does not mention '$needle':"$'\n'"$out_text"
	fi
}

expect_silent_about() {
	local name="$1" needle="$2"
	if [[ "$out_text" != *"$needle"* ]]; then
		pass "$name"
	else
		fail "$name" "output should not mention '$needle':"$'\n'"$out_text"
	fi
}

# --- argument handling ------------------------------------------------------

site args
good_version_dir 1.4.0
alias_pages 1.4.0

run
expect_rc 'missing version rejected' 2
run not-a-version
expect_rc 'malformed version rejected' 2
run v1.4.0 --bogus
expect_rc 'unknown flag rejected' 2

# --- a correctly republished release ----------------------------------------

run v1.4.0
expect_rc 'a version dir naming its own release passes' 0
expect_says 'the alias is reported' '/stable/'
expect_says 'the root redirect is reported' 'stable/'

run 1.4.0
expect_rc 'the version may be given without the v' 0

# The scan is scoped to <article> precisely so these do not fire.
expect_silent_about 'the announce bar is not read as a pin' '9.9.9'
expect_silent_about 'nav chrome is not read as a pin' '3.2.1'
expect_silent_about 'the v2 removal release is exempt' '2.0.0'
expect_silent_about 'a measurement line is not read as a pin' '1.36.1'

# --- the incident: /1.4.0/ still publishing 1.3.0 ---------------------------

site stale
good_version_dir 1.3.0            # what the v1.4.0 tag's tree still said
mkdir -p "$STUB_SITE_ROOT/1.4.0"
cp -R "$STUB_SITE_ROOT/1.3.0/." "$STUB_SITE_ROOT/1.4.0/"
alias_pages 1.4.0

run v1.4.0
expect_rc 'a version dir publishing the previous release fails' 1
# The landing page's only pin is inside a highlighted code block, where the
# theme wraps the leading digit in its own <span> — `1` and `.3.0` are separate
# text nodes. A grep over the raw HTML sees neither, which is why the runbook's
# `curl … | grep -o -- '--version [0-9.]*'` returned nothing on the live site
# instead of reporting the mismatch.
expect_says 'a pin split across highlight spans is still read' '/1.4.0/: advertises 1.3.0'
expect_says 'the stale CRD manifest URL is named' 'advertises v1.3.0'
expect_says 'the stale patch-line hint is named' "patch-line hint \`1.3.z\`"
expect_says 'the fix is pointed at' 'republish from the release'

# Every pin-bearing page is checked, not just the landing page.
for p in '/1.4.0/ ' '/1.4.0/operations/install/' '/1.4.0/operations/upgrade/' '/1.4.0/operations/gitops/'; do
	expect_says "the ${p} page is checked" "$p"
done

# --- the alias and the root -------------------------------------------------

site alias
good_version_dir 1.4.0
alias_pages 1.3.0                 # `stable` still serving the previous release
run v1.4.0
expect_rc 'a stable alias serving another version fails' 1
expect_says 'the alias failure names what it serves' '/stable/ serves http://site/1.3.0/'

run v1.4.0 --no-stable
expect_rc '--no-stable skips the alias, for a backport' 0

site root
good_version_dir 1.4.0
alias_pages 1.4.0 1.2.0           # root redirecting past the alias
run v1.4.0
expect_rc 'a root redirect bypassing stable fails' 1
expect_says 'the root failure names the target' '/ redirects to 1.2.0/'

# --- scans that would otherwise pass by finding nothing ---------------------

site empty
good_version_dir 1.4.0
CANONICAL=1.4.0 page "1.4.0/operations/gitops" "<p>Point Argo CD at the chart.</p>"
alias_pages 1.4.0
run v1.4.0
expect_rc 'a page with no version literal fails' 1
expect_says 'the empty scan is named' 'no release-version literal found'

site noarticle
good_version_dir 1.4.0
alias_pages 1.4.0
printf '<!doctype html><html><body><p>--version 1.4.0</p></body></html>\n' \
	> "$STUB_SITE_ROOT/1.4.0/operations/install/index.html"
run v1.4.0
expect_rc 'a page with no <article> fails' 1
expect_says 'the missing article is named' 'no <article> element'
expect_says 'an unrenderable page reports UNVERIFIED' 'is UNVERIFIED'
expect_silent_about 'an unrenderable page is not called a stale pin' 'do not advertise'

site missing
good_version_dir 1.4.0
alias_pages 1.4.0
rm -rf "$STUB_SITE_ROOT/1.4.0/operations/upgrade"
run v1.4.0
expect_rc 'an unreachable page fails' 1
expect_says 'the unreachable page is named' 'cannot read http://site/1.4.0/operations/upgrade/'

# A page this run could not read is unproven, not wrong. Reporting it as a stale
# pin sends a release engineer re-cutting a branch over a transient 503, which is
# how a real /1.4.0/ run failed while the site was serving one.
expect_says 'an unreadable page reports UNVERIFIED' 'is UNVERIFIED'
expect_silent_about 'an unreadable page is not called a stale pin' 'do not advertise'

# A genuine mismatch alongside an unreadable page still reports the mismatch:
# "unverified" must not mask a pin this run did read and found wrong.
site mixed
good_version_dir 1.3.0
mkdir -p "$STUB_SITE_ROOT/1.4.0"
cp -R "$STUB_SITE_ROOT/1.3.0/." "$STUB_SITE_ROOT/1.4.0/"
alias_pages 1.4.0
rm -rf "$STUB_SITE_ROOT/1.4.0/operations/upgrade"
run v1.4.0
expect_rc 'a mismatch alongside an unreadable page fails' 1
expect_says 'the mismatch is still reported' 'do not advertise'

# --- two runs at once -------------------------------------------------------

# The script removes its whole work directory on exit, so a fixed path lets one
# run delete the pages another is still reading — which is not hypothetical: a
# manual verification overlapping this suite reported four unreachable pages and
# a missing <article> on fixtures that were fine. Both verdicts must survive.
site concurrent_good
good_version_dir 1.4.0
alias_pages 1.4.0
good_root="$STUB_SITE_ROOT"

site concurrent_stale
good_version_dir 1.3.0
mkdir -p "$STUB_SITE_ROOT/1.4.0"
cp -R "$STUB_SITE_ROOT/1.3.0/." "$STUB_SITE_ROOT/1.4.0/"
alias_pages 1.4.0
stale_root="$STUB_SITE_ROOT"

# run_bg SITE_ROOT NAME — start one run, recording its output and exit code.
run_bg() {
	local root="$1" name="$2"
	(
		local rc=0
		CURL="$FIXTURE_DIR/curl" STUB_SITE_ROOT="$root" STUB_CURL_DELAY=0.05 \
			"$SCRIPT" --base http://site v1.4.0 > "$FIXTURE_DIR/$name.out" 2>&1 || rc=$?
		echo "$rc" > "$FIXTURE_DIR/$name.rc"
	) &
}

run_bg "$good_root" good
run_bg "$stale_root" stale
wait

expect_file_rc() {
	local name="$1" want="$2" got
	got="$(< "$FIXTURE_DIR/$name.rc")"
	if [[ "$got" == "$want" ]]; then
		pass "concurrent run: $name (rc=$got)"
	else
		fail "concurrent run: $name" \
			"want rc=$want got rc=$got; output:"$'\n'"$(< "$FIXTURE_DIR/$name.out")"
	fi
}

expect_file_rc good 0
expect_file_rc stale 1

# The overlap must not manufacture a fetch or render failure on either side.
for name in good stale; do
	if grep -qE 'cannot read|no <article> element' "$FIXTURE_DIR/$name.out"; then
		fail "concurrent run: $name reads its own fixtures" \
			"output:"$'\n'"$(< "$FIXTURE_DIR/$name.out")"
	else
		pass "concurrent run: $name reads its own fixtures"
	fi
done

if ((fails > 0)); then
	echo "verify-published-docs-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-published-docs-test: all assertions passed"
