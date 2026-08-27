#!/usr/bin/env bash
#
# verify-pages-live.sh — fail when the live site is still not serving the
# version the run just deployed (Q1000).
#
# Cutting v1.6.0, every signal GitHub produces said the release had published:
# mike pushed the correct tree, `git archive` assembled a correct artifact (the
# uploaded artifact was re-read afterwards and carried 1.6.0 with the stable
# alias), and the Pages deployment reported success and went active. The site
# served the PREVIOUS deployment for roughly 25 minutes regardless, until a
# manual republish. Nothing in the run could observe that, because every check
# it ran read the tree rather than the site.
#
# So this is the half no artifact assertion can cover: it asks the site. Polls
# rather than sampling once, because normal propagation takes seconds and the
# answer that matters is whether it ever arrives.
#
# Scoped to a release version. `dev` redeploys land under a version id that is
# already in versions.json, so there is nothing here that could go red for them
# — see the workflow's own condition.
#
# Usage:
#   verify-pages-live.sh --version V [--alias A] [--base URL]
#                        [--timeout SECONDS] [--interval SECONDS]
#
#   --alias A    Alias this run claimed (e.g. stable). Blank = none claimed.
#   --base URL   Site root to read (default https://actions-gateway.com).
#
# Env:
#   CURL  curl binary to fetch with (default `curl`).
#
# Assertions: verify-pages-live-test.sh, under `make scripts-test`.
set -euo pipefail
shopt -s inherit_errexit

BASE_URL="https://actions-gateway.com"
CURL="${CURL:-curl}"
VERSION=""
ALIAS=""
TIMEOUT=300
INTERVAL=15

while (($# > 0)); do
	case "$1" in
	--version)
		VERSION="${2:-}"
		shift
		;;
	--alias)
		ALIAS="${2:-}"
		shift
		;;
	--base)
		BASE_URL="${2:-}"
		shift
		;;
	--timeout)
		TIMEOUT="${2:-}"
		shift
		;;
	--interval)
		INTERVAL="${2:-}"
		shift
		;;
	*)
		printf 'verify-pages-live: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

if [[ -z "$VERSION" ]]; then
	echo "usage: $0 --version V [--alias A] [--base URL] [--timeout S] [--interval S]" >&2
	exit 2
fi

BASE_URL="${BASE_URL%/}"

WORK_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd)/tmp/verify-pages-live.$$"
mkdir -p "$WORK_DIR"
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

# Each attempt gets its own URL. GitHub Pages caches on the full URL including
# the query string, so a fresh one is a cache miss and reads the origin: without
# it a poll re-reads whatever the edge already holds and would report the stale
# copy for as long as its TTL runs.
cachebust="${GITHUB_RUN_ID:-$$}"

# reason holds why the last attempt did not satisfy the check, so the timeout
# reports what the site was actually serving rather than only that it timed out.
reason=""

# check ATTEMPT — true when the site serves this version. Every miss sets
# `reason` and returns non-zero, so a transient 503 and a stale tree are both
# retried and are distinguishable in the failure.
check() {
	local attempt="$1"
	local q="?cb=${cachebust}-${attempt}"
	local versions="$WORK_DIR/versions.json"
	local page="$WORK_DIR/page.html"

	if ! "$CURL" -fsS --max-time 30 -o "$versions" "$BASE_URL/versions.json$q"; then
		reason="could not read $BASE_URL/versions.json"
		return 1
	fi
	if ! jq -e . "$versions" > /dev/null 2>&1; then
		reason="$BASE_URL/versions.json is not valid JSON"
		return 1
	fi
	if ! jq -e --arg v "$VERSION" 'any(.[]; .version == $v)' "$versions" > /dev/null; then
		reason="$BASE_URL/versions.json does not list $VERSION (serving: $(jq -r 'map(.version) | join(", ")' "$versions"))"
		return 1
	fi
	if [[ -n "$ALIAS" ]] && ! jq -e --arg v "$VERSION" --arg a "$ALIAS" \
		'any(.[]; .version == $v and ((.aliases // []) | index($a) != null))' "$versions" > /dev/null; then
		reason="$BASE_URL/versions.json does not put '$ALIAS' on $VERSION (it is on: $(jq -r --arg a "$ALIAS" '[.[] | select((.aliases // []) | index($a) != null) | .version] | join(", ") | if . == "" then "<no version>" else . end' "$versions"))"
		return 1
	fi
	# versions.json is one small file and could in principle land ahead of the
	# tree it describes, so read a page out of the version directory too — that
	# is the 404 a visitor following the release notes actually hits.
	if ! "$CURL" -fsS --max-time 30 -o "$page" "$BASE_URL/$VERSION/$q"; then
		reason="$BASE_URL/$VERSION/ is not reachable"
		return 1
	fi
	return 0
}

attempt=0
while :; do
	attempt=$((attempt + 1))
	if check "$attempt"; then
		printf 'verify-pages-live: %s/%s/ is live%s after %ds (attempt %d)\n' \
			"$BASE_URL" "$VERSION" "${ALIAS:+ as $ALIAS}" "$SECONDS" "$attempt"
		exit 0
	fi
	if ((SECONDS + INTERVAL >= TIMEOUT)); then
		break
	fi
	printf 'verify-pages-live: attempt %d at %ds: %s; retrying in %ds\n' \
		"$attempt" "$SECONDS" "$reason" "$INTERVAL"
	sleep "$INTERVAL"
done

# Report the elapsed time rather than the budget. The loop stops STARTING
# attempts at TIMEOUT, so one already in flight runs to its own curl deadline
# and the run legitimately overshoots; printing the budget here reads as a
# precision the check does not have.
printf '::error::verify-pages-live: %s did not serve %s after %ds (%ds budget, %d attempt(s)): %s\n' \
	"$BASE_URL" "$VERSION" "$SECONDS" "$TIMEOUT" "$attempt" "$reason" >&2
printf '::error::verify-pages-live: the deploy reported success, so the artifact reached Pages and the site did not. Re-run this workflow (Actions -> pages -> Run workflow) with version=%s%s to republish, then confirm with: make verify-published-docs VERSION=v%s\n' \
	"$VERSION" "${ALIAS:+ alias=$ALIAS set_default=true}" "$VERSION" >&2
exit 1
