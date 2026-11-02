#!/usr/bin/env bash
# check-runner-window.sh — is the pinned actions/runner close to aging out of
# GitHub's 30-day update window?
#
#   scripts/release/check-runner-window.sh [--pinned X.Y.Z] [--warn-days N] [--now EPOCH] [RELEASES_JSON]
#
# GitHub stops queueing jobs to a self-hosted runner that has not updated "within
# 30 days of a new version being made available", and does not refuse the
# connection, so a stale pin shows up as every job lapsing and not as an error
# (Q1149). The window is counted here from the `published_at` of the FIRST stable
# release newer than the pin, which is the documented reading and the stricter of
# the two readings the dogfood runs allow; a newer-still release never restarts it.
#
# RELEASES_JSON is the array `gh api repos/actions/runner/releases` returns, from
# a file or stdin (`-`). Drafts and prereleases are skipped: GitHub does not offer
# a prerelease as the new version, and counting one would start the clock early.
# The pin defaults to `RunnerVersion` in cmd/agc/names/names.go.
#
# Exit 0 when the pin is the newest stable release or is inside the warning
# threshold, 1 when it is not, 2 when the check could not measure (no input, an
# unparseable pin, a release with no timestamp). 2 is never 1: a feed that failed
# to load must not read as a stale pin, nor as a healthy one.
set -euo pipefail
shopt -s inherit_errexit

WINDOW_DAYS=30
warn_days=21
pinned=""
now=""
input="-"

usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d'; }

while (($#)); do
	case "$1" in
	--pinned) pinned="${2:?--pinned needs a version}"; shift 2 ;;
	--warn-days) warn_days="${2:?--warn-days needs a number}"; shift 2 ;;
	--now) now="${2:?--now needs an epoch}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) input="$1"; shift ;;
	esac
done

die() { printf 'check-runner-window: %s\n' "$*" >&2; exit 2; }

if [[ -z "$pinned" ]]; then
	names_go="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/cmd/agc/names/names.go"
	pinned="$(awk -F'"' '/^const RunnerVersion = "/ { print $2; exit }' "$names_go")"
fi
[[ "$pinned" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "pinned version '${pinned}' is not X.Y.Z"
[[ "$warn_days" =~ ^[0-9]+$ ]] || die "--warn-days '${warn_days}' is not a number"
now="${now:-$(date +%s)}"
[[ "$now" =~ ^[0-9]+$ ]] || die "--now '${now}' is not an epoch"

if [[ "$input" == "-" ]]; then
	releases="$(cat)"
else
	[[ -r "$input" ]] || die "cannot read ${input}"
	releases="$(<"$input")"
fi

# One jq call: the earliest publication among stable releases newer than the pin,
# or null. Versions compare as integer arrays, so 2.10.0 sorts above 2.9.0.
verdict="$(jq -r --arg pinned "$pinned" --argjson now "$now" '
	def key: ltrimstr("v") | split(".") | map(tonumber);
	if (type != "array" or length == 0) then error("no releases in the feed") else . end
	| [ .[]
	    | select(.draft == false and .prerelease == false)
	    | select((.tag_name | ltrimstr("v") | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")))
	    | select((.tag_name | key) > ($pinned | key))
	    | { tag: .tag_name, at: (.published_at // error("release " + .tag_name + " has no published_at")) } ]
	| sort_by(.at | fromdateiso8601)
	| if length == 0 then "none"
	  else .[0] as $f
	  | [$f.tag, $f.at, (($now - ($f.at | fromdateiso8601)) / 86400 | floor)] | @tsv
	  end
' <<<"$releases")" || die "could not read the releases feed"
[[ -n "$verdict" ]] || die "could not read the releases feed: it was empty"

if [[ "$verdict" == "none" ]]; then
	printf 'actions/runner %s is the newest stable release; no update window is open.\n' "$pinned"
	exit 0
fi

IFS=$'\t' read -r first_tag first_at days <<<"$verdict"
left=$((WINDOW_DAYS - days))
printf 'actions/runner %s is pinned; the first newer stable release, %s, was published %s, %d days ago; %d days remain of the %d-day window.\n' \
	"$pinned" "$first_tag" "$first_at" "$days" "$left" "$WINDOW_DAYS"
if ((days >= warn_days)); then
	printf 'STALE: bump names.RunnerVersion and the dogfood worker images to %s or newer before day %d.\n' "$first_tag" "$WINDOW_DAYS"
	exit 1
fi
exit 0
