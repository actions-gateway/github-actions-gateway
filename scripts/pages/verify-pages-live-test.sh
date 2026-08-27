#!/usr/bin/env bash
#
# Assert what verify-pages-live.sh catches (Q1000).
#
# The gate reads a live site, so the suite runs the real script end to end behind
# a stub `curl` serving a fixture site — the same shape verify-published-docs-
# test.sh uses, and for the same reason: asserting the JSON handling alone leaves
# the poll loop, the cache-busting and the timeout unexecuted.
#
# The positive control is the v1.6.0 incident: versions.json listing 1.5.0 as
# `stable` with no 1.6.0 entry, served from a run whose artifact was correct.
# Every tree-side check in that run was green, so a suite that only proved the
# artifact assertion would be proving the half that did not fail.
#
# Two cases carry the loop rather than the comparison. A site that goes correct
# on a later attempt must pass, or the check would report a red for ordinary
# propagation; and the stub records the query string of every request, because a
# poll that re-reads one cached URL would report the stale copy for as long as
# the edge holds it — which is exactly what the incident's manual cache-busting
# had to work around.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

SCRIPT="$REPO_ROOT/scripts/pages/verify-pages-live.sh"
FIXTURE_DIR="$REPO_ROOT/tmp/verify-pages-live-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# Stub curl: resolve the requested URL under $STUB_SITE_ROOT, logging the query
# string of every request. $STUB_FLIP_AFTER attempts serve the `before` tree and
# the rest serve `after`, which is how the propagation case is expressed.
cat > "$FIXTURE_DIR/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
dest=""
url=""
while (($# > 0)); do
	case "$1" in
	-o)
		dest="$2"
		shift
		;;
	--max-time) shift ;;
	-*) ;;
	*) url="$1" ;;
	esac
	shift
done
path="${url#http://site}"
query="${path#*\?}"
path="${path%%\?*}"
printf '%s\n' "$query" >> "$STUB_QUERY_LOG"

tree="after"
if [[ -n "${STUB_FLIP_AFTER:-}" ]]; then
	# One line per request, and versions.json is the first read of each attempt.
	attempts="$(grep -c . "$STUB_QUERY_LOG")"
	((attempts > STUB_FLIP_AFTER)) || tree="before"
fi

src="$STUB_SITE_ROOT/$tree${path}"
[[ "$src" == */ ]] && src="${src}index.html"
[[ -f "$src" ]] || exit 22
cp "$src" "$dest"
STUB
chmod +x "$FIXTURE_DIR/curl"

# tree NAME VERSIONS_JSON DIR... — write one fixture site.
tree() {
	local name="$1" versions="$2"
	shift 2
	local root="$STUB_SITE_ROOT/$name"
	mkdir -p "$root"
	printf '%s\n' "$versions" > "$root/versions.json"
	local d
	for d in "$@"; do
		mkdir -p "$root/$d"
		echo "<html>$d</html>" > "$root/$d/index.html"
	done
}

# expect NAME EXPECTED_RC ARGS... — run the script behind the stub.
expect() {
	local name="$1" want="$2"
	shift 2
	local got=0
	: > "$STUB_QUERY_LOG"
	CURL="$FIXTURE_DIR/curl" PATH="$PATH" \
		"$SCRIPT" "$@" > "$FIXTURE_DIR/out.log" 2>&1 || got=$?
	if ((got == want)); then
		pass "$name"
	else
		fail "$name" "expected exit $want, got $got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
	fi
}

CORRECT='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.6.0","title":"1.6.0","aliases":["stable"]},
{"version":"1.5.0","title":"1.5.0","aliases":[]}]'

STALE='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.5.0","title":"1.5.0","aliases":["stable"]}]'

HALF='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.6.0","title":"1.6.0","aliases":[]},
{"version":"1.5.0","title":"1.5.0","aliases":["stable"]}]'

export STUB_SITE_ROOT="$FIXTURE_DIR/site"
export STUB_QUERY_LOG="$FIXTURE_DIR/queries.log"
mkdir -p "$STUB_SITE_ROOT"

tree after "$CORRECT" dev 1.6.0 1.5.0 stable
tree before "$STALE" dev 1.5.0 stable

# --- the site is already serving the release ---
expect "a live site serving the version passes" 0 \
	--version 1.6.0 --alias stable --base http://site --timeout 4 --interval 1

# --- the positive control: the v1.6.0 incident ---
export STUB_FLIP_AFTER=9999
expect "a site still serving the previous release fails" 1 \
	--version 1.6.0 --alias stable --base http://site --timeout 4 --interval 1
if grep -q 'does not list 1.6.0' "$FIXTURE_DIR/out.log" &&
	grep -q 'Re-run this workflow' "$FIXTURE_DIR/out.log"; then
	pass "the failure names what the site served and how to republish"
else
	fail "the failure names what the site served and how to republish" \
		"got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
fi

unset STUB_FLIP_AFTER

# --- propagation that arrives late is a pass, not a red run ---
#
# The request-count assertions live HERE rather than on the timeout case above,
# and both halves of that matter.
#
# Deterministic: the stub serves the stale tree for the first two requests
# whatever the clock does, so reaching success REQUIRES at least three. Counting
# on the timeout case instead made the assertion a race against the budget —
# under a loaded `make scripts-test` (99 suites in parallel) a single attempt
# consumed the whole 4s, the loop broke after one request, and the suite failed
# for a reason that was never about the code.
#
# And with one request the distinctness check passed VACUOUSLY: one query string
# is trivially all-distinct. So the floor below is what gives that assertion
# anything to prove, rather than a tidier way to state it.
#
# The budget is generous because this case must SUCCEED, and success exits the
# loop as soon as the site is right. A large timeout costs nothing on that path
# and removes the clock from the assertion entirely.
export STUB_FLIP_AFTER=2
: > "$STUB_QUERY_LOG"
expect "a site that goes correct on a later attempt passes" 0 \
	--version 1.6.0 --alias stable --base http://site --timeout 60 --interval 1

attempts="$(grep -c . "$STUB_QUERY_LOG")"
if ((attempts >= 3)); then
	pass "the check polls rather than sampling once ($attempts requests)"
else
	fail "the check polls rather than sampling once" "made $attempts request(s)"
fi
# Per ATTEMPT, not per request. The two fetches inside one attempt share a
# token by design and may: they are different URLs, so they are already
# different cache keys. What must never repeat is the token across attempts,
# which is what a stale edge copy would otherwise be re-read through. Asserting
# one-per-request looked stricter and was simply wrong -- it only ever passed
# because the timeout case makes exactly one request per attempt.
distinct="$(sort -u "$STUB_QUERY_LOG" | grep -c .)"
if ((distinct >= 3)); then
	pass "each attempt carries its own cache-busting query ($distinct distinct over $attempts request(s))"
else
	fail "each attempt carries its own cache-busting query" \
		"$attempts request(s) used only $distinct distinct query string(s)"
fi
unset STUB_FLIP_AFTER

# --- the alias half, which the version check alone cannot see ---
tree before "$HALF" dev 1.6.0 1.5.0 stable
export STUB_FLIP_AFTER=9999
expect "a site whose alias is still on the previous release fails" 1 \
	--version 1.6.0 --alias stable --base http://site --timeout 4 --interval 1
expect "the same site passes for a deploy that claimed no alias" 0 \
	--version 1.6.0 --base http://site --timeout 4 --interval 1

# The workflow passes the alias mike claimed verbatim, so a deploy that claimed
# none calls this with an empty --alias rather than omitting the flag.
expect "an explicitly empty --alias is not a claim" 0 \
	--version 1.6.0 --alias "" --base http://site --timeout 4 --interval 1
unset STUB_FLIP_AFTER

# --- versions.json can be right while the version tree is not served ---
tree after "$CORRECT" dev 1.5.0 stable
rm -rf "$STUB_SITE_ROOT/after/1.6.0"
expect "a listed version whose pages 404 fails" 1 \
	--version 1.6.0 --alias stable --base http://site --timeout 4 --interval 1
if grep -q 'is not reachable' "$FIXTURE_DIR/out.log"; then
	pass "the unreachable version directory is named"
else
	fail "the unreachable version directory is named" \
		"got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
fi

# --- an unreadable site is unproven, not clean ---
rm -f "$STUB_SITE_ROOT/after/versions.json"
expect "an unreadable versions.json fails" 1 \
	--version 1.6.0 --alias stable --base http://site --timeout 4 --interval 1

# --- usage ---
expect "a missing --version is a usage error" 2 --base http://site

if ((fails > 0)); then
	echo "verify-pages-live-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-pages-live-test: all assertions passed"
