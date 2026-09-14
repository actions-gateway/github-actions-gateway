#!/usr/bin/env bash
#
# Render a window's soak readings as the markdown rows that go into the v2 GA
# plan's Phase 1 readings table.
#
# WHY THIS EXISTS. The readings are the durable product of a booked dogfood
# window — the scarcest thing in this release process — and until this script
# they survived only as lines in whatever terminal the gate ran in. A window
# costs real money and cannot be replayed, so a reading that scrolled past was
# gone, and the transcription into the plan was a human reading prose back and
# retyping it. Both halves of that are now mechanical: validate-release.sh
# appends one JSON record per reading, and this renders them.
#
# WHAT IT DELIBERATELY DOES NOT DO. It does not edit the plan. A reading is
# evidence and the criteria table is an argument about what the evidence means,
# so a human decides whether criterion 2 is now met; this only removes the
# retyping. It also never invents a row for a reading that is absent: a window
# that took two of three readings renders two rows, and the missing one is
# visible by its absence rather than papered over with an empty cell.
#
# Usage:
#   scripts/dogfood/soak-readings.sh [--file PATH] [--format rows|json]
#
#   --file    the readings stream (default: tmp/soak-readings.jsonl, the path
#             validate-release.sh writes through RELEASE_READINGS_FILE)
#   --format  rows (default) markdown table rows; json the raw records
#
# Exit: 0 rows rendered, 1 the stream holds no readings or the usage is bad.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

READINGS_FILE="${REPO_ROOT}/tmp/soak-readings.jsonl"
FORMAT="rows"

usage() { sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'; }

while (($#)); do
	case "$1" in
	--file)
		[[ -n "${2:-}" ]] || die "--file needs a path"
		READINGS_FILE="$2"
		shift 2
		;;
	--format)
		[[ "${2:-}" =~ ^(rows|json)$ ]] || die "--format takes rows or json"
		FORMAT="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
done

require_cmd jq "https://jqlang.github.io/jq/download/"

[[ -r "${READINGS_FILE}" ]] || {
	echo "no readings at ${READINGS_FILE}" >&2
	echo "  A window writes it; if one has run, check RELEASE_READINGS_FILE." >&2
	exit 1
}

# Last record per id wins. A gate re-run inside one window appends rather than
# truncating — deliberately, so a re-run cannot destroy the earlier reading —
# and the newest record is the one that describes the cluster now.
records="$(jq -sc '
	map(select(.kind == "reading"))
	| group_by(.id)
	| map(max_by(.t))
	| sort_by(.id)' "${READINGS_FILE}")"

count="$(jq -r 'length' <<<"${records}")"
((count > 0)) || {
	echo "no readings in ${READINGS_FILE}" >&2
	exit 1
}

if [[ "${FORMAT}" == "json" ]]; then
	jq '.' <<<"${records}"
	exit 0
fi

# The verdict vocabulary is closed (progress.sh § progress_reading), so an
# unknown one is rendered loudly rather than blankly: it means the writer and
# this renderer have drifted apart, and a blank cell would hide that behind
# something that looks like a formatting slip.
#
# `not-taken` gets its own symbol and is never a failure. A window that could
# not produce a reading has said nothing about the criterion, so rendering it
# as ❌ would be a claim the evidence does not support.
#
# A literal `|` in a detail ends the markdown cell early and silently shifts
# every column after it, so it is escaped here. The detail text comes from
# kubectl and gcloud output, which this repo does not control.
jq -r '
	def cell: tostring | gsub("\\|"; "\\|");
	.[] |
	(if   .verdict == "pass"      then "✅ Taken, positive"
	 elif .verdict == "finding"   then "⚠️ Taken, negative"
	 elif .verdict == "not-taken" then "🔲 Not taken"
	 else "❓ unknown verdict \(.verdict)" end) as $v |
	"| \(.id | cell) | \(.criterion | cell) | \($v) | \(.detail | cell) | `\(.rc)` on `\(.cluster)`, \(.t | strftime("%Y-%m-%d")) |"
	' <<<"${records}"
