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
# Three cases carry the loop rather than the comparison. A site that goes correct
# on a later attempt must pass, or the check would report a red for ordinary
# propagation; the loop's own attempt counter says it retried rather than
# sampling once; and the stub records the query string of every request, because
# a poll that re-reads one cached URL would report the stale copy for as long as
# the edge holds it, which is exactly what the incident's manual cache-busting
# had to work around. None of the three may rest on wall clock (Q1034).
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
# string of every request. $STUB_FLIP_AFTER requests serve the `before` tree and
# the rest serve `after`, which is how the propagation case is expressed.
# $STUB_GIVE_UP_AFTER bounds a loop that never converges, answering with
# $STUB_GIVE_UP_VERSIONS past the cap; see its comment below.
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
	requests="$(grep -c . "$STUB_QUERY_LOG")"
	((requests > STUB_FLIP_AFTER)) || tree="before"
	# Non-convergence guard, counted in REQUESTS so it can never become a second
	# clock. A case driven with a budget no run can reach would otherwise poll
	# for a day if the site never went correct. Past the cap the stub answers
	# synthetically so the loop ends whatever state the fixture is in -- serving
	# the `after` tree instead would be trusting the very thing that stopped
	# converging. The caller's exact-attempt assertion is what then reports the
	# runaway, because the count will be past what that case expects.
	if [[ -n "${STUB_GIVE_UP_AFTER:-}" ]] && ((requests > STUB_GIVE_UP_AFTER)); then
		if [[ "$path" == /versions.json ]]; then
			printf '%s\n' "${STUB_GIVE_UP_VERSIONS:-[]}" > "$dest"
		else
			echo "<html>give-up</html>" > "$dest"
		fi
		exit 0
	fi
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
# All three assertions here are about HOW MANY attempts the loop makes, so none
# of them may be a function of the clock. The stub supplies the deterministic
# half: it serves the stale tree for the first two requests whatever the host
# does, so success requires a third attempt.
#
# The budget supplies the other half, and it is set where no run can reach it
# rather than merely raised. The loop stops starting attempts at
# `SECONDS + INTERVAL >= TIMEOUT`, and behind a stubbed curl that expression
# measures the host instead of the subject: under a loaded `make scripts-test`
# one simulated attempt has cost 606s, which broke the previous 60s budget after
# a single attempt and collapsed all three assertions at once (Q1034). Raising
# the budget only moves that threshold. What removes it is a budget the run
# cannot reach. $STUB_GIVE_UP_AFTER caps this case at 8 requests, so a runaway
# ends by the tenth and even at that 606s attempt the case is bounded near
# 6,000s against a budget of a day, and the assertions below are counted in
# attempts rather than seconds.
#
# Should this case fail anyway, its log names the budget: a `(86400s budget,
# 1 attempt(s))` line is the clock binding again, not a retry defect.
export STUB_FLIP_AFTER=2
export STUB_GIVE_UP_AFTER=8
export STUB_GIVE_UP_VERSIONS="$CORRECT"
expect "a site that goes correct on a later attempt passes" 0 \
	--version 1.6.0 --alias stable --base http://site --timeout 86400 --interval 1

# The loop's OWN counter, read off the success line, rather than a count of stub
# requests: attempts are what this case is about, and one attempt makes one or
# two requests depending on how far through `check` it gets. Exact rather than a
# floor: the stub fixes the answer at 3, so a floor would accept a loop that had
# stopped bounding itself as readily as one that works.
made="$(awk '/is live/ { sub(/.*\(attempt /, ""); sub(/\).*/, ""); print }' "$FIXTURE_DIR/out.log")"
if [[ "$made" == "3" ]]; then
	pass "the check polls rather than sampling once (3 attempts)"
else
	fail "the check polls rather than sampling once" \
		"the stub served the stale tree for two requests, so success needed attempt 3; got '${made:-no success line}'"
fi

# Per ATTEMPT, not per request. The two fetches inside one attempt share a
# token by design and may: they are different URLs, so they are already
# different cache keys. What must never repeat is the token across attempts,
# which is what a stale edge copy would otherwise be re-read through. Asserting
# one-per-request looked stricter and was simply wrong -- it only ever passed
# because the timeout case makes exactly one request per attempt.
#
# Tied to the attempt count above so it cannot pass vacuously: over a single
# attempt one query string is trivially all-distinct.
requests="$(grep -c . "$STUB_QUERY_LOG")"
distinct="$(sort -u "$STUB_QUERY_LOG" | grep -c .)"
if [[ "$made" == "3" ]] && ((distinct == 3)); then
	pass "each attempt carries its own cache-busting query (3 distinct over $requests request(s))"
else
	fail "each attempt carries its own cache-busting query" \
		"$requests request(s) over ${made:-0} attempt(s) used $distinct distinct query string(s)"
fi
unset STUB_FLIP_AFTER STUB_GIVE_UP_AFTER STUB_GIVE_UP_VERSIONS

# The cap above is the only thing standing between a budget no run can reach and
# a day-long hang, and a green suite cannot show that it was read: every case so
# far converges long before 8 requests. So drive a site that never goes correct
# and require the cap to end the run at the attempt it predicts -- eight stale
# attempts, then the synthetic answer on the ninth.
export STUB_FLIP_AFTER=9999
export STUB_GIVE_UP_AFTER=8
export STUB_GIVE_UP_VERSIONS="$CORRECT"
expect "a site that never goes correct still terminates" 0 \
	--version 1.6.0 --alias stable --base http://site --timeout 86400 --interval 0
capped="$(awk '/is live/ { sub(/.*\(attempt /, ""); sub(/\).*/, ""); print }' "$FIXTURE_DIR/out.log")"
if [[ "$capped" == "9" ]]; then
	pass "the request cap ends a run that never converges (attempt 9)"
else
	fail "the request cap ends a run that never converges" \
		"the cap is 8 requests and each stale attempt makes one, so the run should end on attempt 9; got '${capped:-no success line}'"
fi
unset STUB_FLIP_AFTER STUB_GIVE_UP_AFTER STUB_GIVE_UP_VERSIONS

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
