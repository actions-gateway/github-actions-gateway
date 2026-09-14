#!/usr/bin/env bash
#
# Assertions for the soak-reading writer (progress.sh § progress_reading) and
# the renderer that turns a window's readings into plan table rows.
#
# These two are the only durable record of a dogfood window's v2 GA evidence.
# A window is billable and cannot be replayed, so every failure mode here is
# expensive and quiet: a writer that drops a record loses evidence nothing will
# notice is gone, a renderer that mislabels a verdict writes a claim into the
# plan that the reading did not support, and a `not-taken` rendered as a failure
# turns "we did not measure this" into "we measured it and it was bad".
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

WORK="${REPO_ROOT}/tmp/soak-readings-test.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

RENDER="${REPO_ROOT}/scripts/dogfood/soak-readings.sh"

fails=0

ok() { printf 'ok   %-56s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-56s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

want_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$name" "$(printf '%q' "$got")"
	else
		bad "$name" "want $(printf '%q' "$want") got $(printf '%q' "$got")"
	fi
}

want_contains() {
	local name="$1" needle="$2" hay="$3"
	if [[ "$hay" == *"$needle"* ]]; then
		ok "$name" "contains $(printf '%q' "$needle")"
	else
		bad "$name" "missing $(printf '%q' "$needle") in $(printf '%q' "$hay")"
	fi
}

want_lacks() {
	local name="$1" needle="$2" hay="$3"
	if [[ "$hay" != *"$needle"* ]]; then
		ok "$name" "lacks $(printf '%q' "$needle")"
	else
		bad "$name" "unexpected $(printf '%q' "$needle") in $(printf '%q' "$hay")"
	fi
}

# ---------------------------------------------------------------------------
# The writer. Sourced with both file variables pointed at the scratch dir
# BEFORE the source, because the lib derives its paths at source time.
# ---------------------------------------------------------------------------

RELEASE_PROGRESS_FILE="${WORK}/progress.jsonl"
RELEASE_STATUS_FILE="${WORK}/status.json"
export RELEASE_PROGRESS_FILE RELEASE_STATUS_FILE
# shellcheck source=scripts/dogfood/lib/progress.sh
source "${REPO_ROOT}/scripts/dogfood/lib/progress.sh"

want_eq "readings file derives beside the phase stream" \
	"${WORK}/soak-readings.jsonl" "${RELEASE_READINGS_FILE}"

GAG_IMAGE_TAG="v9.9.9-rc.7"
CLUSTER="gag-test"
export GAG_IMAGE_TAG CLUSTER

progress_reading Q1059 "v2beta1 serves every kind" pass "five kinds served"
rec="$(cat "${RELEASE_READINGS_FILE}")"
want_eq "one call writes one line" 1 "$(wc -l <"${RELEASE_READINGS_FILE}" | tr -d ' ')"
want_eq "record kind" "reading" "$(jq -r '.kind' <<<"$rec")"
want_eq "record id" "Q1059" "$(jq -r '.id' <<<"$rec")"
want_eq "record criterion" "v2beta1 serves every kind" "$(jq -r '.criterion' <<<"$rec")"
want_eq "record verdict" "pass" "$(jq -r '.verdict' <<<"$rec")"
want_eq "record detail" "five kinds served" "$(jq -r '.detail' <<<"$rec")"
want_eq "record stamps the rc under test" "v9.9.9-rc.7" "$(jq -r '.rc' <<<"$rec")"
want_eq "record stamps the cluster" "gag-test" "$(jq -r '.cluster' <<<"$rec")"

# Appending rather than truncating is the property that makes the evidence
# survive: a second gate run inside one window must not destroy the first run's
# reading, because the window is what was paid for and the run is not.
progress_reading Q1060 "v2alpha1 round-trips" not-taken "two API groups"
want_eq "a second reading appends" 2 "$(wc -l <"${RELEASE_READINGS_FILE}" | tr -d ' ')"
want_eq "the first record survives the second" "Q1059" \
	"$(head -1 "${RELEASE_READINGS_FILE}" | jq -r '.id')"

# Absent detail is legal; the renderer must not be handed a null.
progress_reading Q1048 "mirror census" pass
want_eq "detail defaults to empty, not null" "" \
	"$(tail -1 "${RELEASE_READINGS_FILE}" | jq -r '.detail')"

# The stream and the readings are separate files on purpose: a phase transition
# must not land in the evidence, and a reading must not be reset when the stream
# is. progress_event truncates the stream at gate start; readings never reset.
progress_event soak start "taking readings"
want_eq "a phase event does not reach the readings file" 3 \
	"$(wc -l <"${RELEASE_READINGS_FILE}" | tr -d ' ')"
want_lacks "a reading does not reach the phase stream" '"kind":"reading"' \
	"$(cat "${RELEASE_PROGRESS_FILE}")"

# The point of the whole split. progress_init truncates the stream at the start
# of every run; if it reached the readings too, a re-run of the gate inside one
# window would erase the evidence that window was booked to produce.
progress_init
want_eq "progress_init resets the phase stream" 0 \
	"$(wc -c <"${RELEASE_PROGRESS_FILE}" | tr -d ' ')"
want_eq "progress_init leaves the readings intact" 3 \
	"$(wc -l <"${RELEASE_READINGS_FILE}" | tr -d ' ')"

# ---------------------------------------------------------------------------
# The renderer.
# ---------------------------------------------------------------------------

rows="$(bash "$RENDER" --file "${RELEASE_READINGS_FILE}")"
want_eq "renders one row per reading" 3 "$(wc -l <<<"$rows" | tr -d ' ')"
want_contains "pass renders as taken and positive" "| Q1059 | v2beta1 serves every kind | ✅ Taken, positive | five kinds served |" "$rows"
want_contains "not-taken renders as its own symbol" "| Q1060 | v2alpha1 round-trips | 🔲 Not taken | two API groups |" "$rows"
want_lacks "not-taken is never rendered as a failure" "❌" "$rows"
# Backticks, so the needle is built rather than written as a literal: a single
# quoted one reads to shellcheck as an unexpanded expansion.
tick='`'
want_contains "rows carry the rc and cluster provenance" \
	"${tick}v9.9.9-rc.7${tick} on ${tick}gag-test${tick}" "$rows"
want_eq "rows sort by id" "Q1048" "$(head -1 <<<"$rows" | awk -F'|' '{gsub(/ /,"",$2); print $2}')"
want_lacks "the phase event is not rendered as a row" "taking readings" "$rows"

# A finding is evidence, not a gate failure: the window measured the thing and
# the answer was negative, which is exactly what a soak reading is for.
mixed="${WORK}/mixed.jsonl"
jq -cn '{kind:"reading",t:1757000000,id:"Q1",criterion:"c",verdict:"finding",detail:"drifted",rc:"v1",cluster:"k"}' >"$mixed"
jq -cn '{kind:"phase",t:1757000001,phase:"soak",state:"done"}' >>"$mixed"
mixed_rows="$(bash "$RENDER" --file "$mixed")"
want_contains "finding renders as taken and negative" "⚠️ Taken, negative" "$mixed_rows"
want_eq "a phase record beside a reading is skipped" 1 "$(wc -l <<<"$mixed_rows" | tr -d ' ')"

# Last record per id wins. A gate re-run inside one window appends a second
# record for the same reading, and the newest is the one describing the cluster.
dup="${WORK}/dup.jsonl"
jq -cn '{kind:"reading",t:1757000000,id:"Q1",criterion:"c",verdict:"not-taken",detail:"first",rc:"v1",cluster:"k"}' >"$dup"
jq -cn '{kind:"reading",t:1757000900,id:"Q1",criterion:"c",verdict:"pass",detail:"second",rc:"v1",cluster:"k"}' >>"$dup"
dup_rows="$(bash "$RENDER" --file "$dup")"
want_eq "a re-read collapses to one row" 1 "$(wc -l <<<"$dup_rows" | tr -d ' ')"
want_contains "the newest record wins" "second" "$dup_rows"
want_lacks "the superseded record is not rendered" "first" "$dup_rows"

# An unknown verdict means the writer and the renderer have drifted. It has to
# be loud: a blank cell reads as a formatting slip and gets pasted into the plan.
odd="${WORK}/odd.jsonl"
jq -cn '{kind:"reading",t:1757000000,id:"Q1",criterion:"c",verdict:"maybe",detail:"d",rc:"v1",cluster:"k"}' >"$odd"
want_contains "an unknown verdict is rendered loudly" "❓ unknown verdict maybe" \
	"$(bash "$RENDER" --file "$odd")"

# A `|` in a detail would end the cell early and shift every column after it.
# The detail comes from kubectl and gcloud output, which this repo does not own.
pipey="${WORK}/pipe.jsonl"
jq -cn '{kind:"reading",t:1757000000,id:"Q1",criterion:"c",verdict:"pass",detail:"a|b",rc:"v1",cluster:"k"}' >"$pipey"
pipe_row="$(bash "$RENDER" --file "$pipey")"
want_contains "a pipe in a detail is escaped" 'a\|b' "$pipe_row"
# Six delimiters bound five cells. Counted after the escaped pipe is removed,
# because an escape that did not take would read as a sixth cell here.
unescaped="${pipe_row//\\|/}"
pipes="${unescaped//[^|]/}"
want_eq "the escaped row still has five cells" 6 "${#pipes}"

# Exit status is the difference between "the window produced nothing" and "the
# renderer rendered an empty table", which look identical on stdout.
set +e
bash "$RENDER" --file "${WORK}/absent.jsonl" >"${WORK}/absent.out" 2>&1
want_eq "a missing stream exits non-zero" 1 "$?"
set -e
want_contains "a missing stream says where it looked" "absent.jsonl" "$(cat "${WORK}/absent.out")"

: >"${WORK}/empty.jsonl"
set +e
bash "$RENDER" --file "${WORK}/empty.jsonl" >/dev/null 2>&1
want_eq "an empty stream exits non-zero" 1 "$?"
set -e

phase_only="${WORK}/phase-only.jsonl"
jq -cn '{kind:"phase",t:1757000000,phase:"soak",state:"done"}' >"$phase_only"
set +e
bash "$RENDER" --file "$phase_only" >/dev/null 2>&1
want_eq "a stream of phase events alone exits non-zero" 1 "$?"
set -e

want_eq "--format json returns the deduped records" "Q1" \
	"$(bash "$RENDER" --file "$dup" --format json | jq -r '.[0].id')"
want_eq "--format json returns one record per id" 1 \
	"$(bash "$RENDER" --file "$dup" --format json | jq -r 'length')"

set +e
bash "$RENDER" --file "$dup" --format yaml >/dev/null 2>&1
want_eq "an unknown --format is rejected" 1 "$?"
set -e

# ---------------------------------------------------------------------------
# Disabling. A caller that does not want readings written sets the variable
# empty, the same escape hatch RELEASE_STATUS_FILE has.
# ---------------------------------------------------------------------------

RELEASE_READINGS_FILE=""
progress_reading Q1 c pass d
want_eq "an empty readings file disables the writer" 3 \
	"$(wc -l <"${WORK}/soak-readings.jsonl" | tr -d ' ')"

if ((fails > 0)); then
	echo "FAILED: ${fails} assertion(s)" >&2
	exit 1
fi
echo "PASS: soak-readings"
