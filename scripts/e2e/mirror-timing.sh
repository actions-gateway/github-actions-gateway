#!/usr/bin/env bash
#
# mirror-timing.sh — is a registry-mirror cache hit distinguishable from a miss
# FROM INSIDE A KATA GUEST, across the bridge NAT, on the shipped Deployment
# shape? The probe Q1020 asks for.
#
# WHY IT RUNS IN THE JOB. The same reason egress-negatives.sh does, beside which
# it runs: the claim is about a Kata worker, whose inner containers reach the
# network through a bridge NAT inside a micro-VM guest, and a plain pod sits
# somewhere else on that path. An earlier reading taken from a laptop over the
# public internet bounded the channel rather than measuring it where an attacker
# sits, and that is the gap this closes.
#
# WHAT IT DOES NOT DECIDE. Nothing about the mirror topology. `/v2/_catalog` is
# refused on every instance (Q1022), so timing is now the whole of what a shared
# cache exposes rather than the smaller half of it — which raises what this
# reading is worth without letting it settle the choice. A confirmed channel
# cannot make a shared set private and a refuted one cannot make it more so, and
# an operator whose tenants must not learn what each other build should take the
# isolated topology without waiting for this. See
# docs/operations/kata-dind-workloads.md#choosing-a-mirror-topology.
#
# HOW THE ARMS ARE BUILT, and why not the way the row first framed it. A warm
# repository against a cold one gives two arms of different blobs, so every
# sample carries its blob's size as well as its cache state. Here each reference
# contributes BOTH arms: fetched once cold (a miss) and once again later (a hit),
# so the pair differs only in cache state. Blob size, which dominates transfer
# time, is held fixed by construction rather than argued away.
#
# The fetches are interleaved rather than run as two passes, because two passes
# put every miss before every hit and any drift in the network over the run
# lands entirely on the difference. Each hit is deliberately NOT taken
# immediately after its own miss either, which would time the most favourable
# moment a hit ever has.
#
# COLDNESS CANNOT BE VERIFIED FROM HERE, and pretending otherwise is the way this
# probe would lie. The references below are chosen because the suite does not
# pull them, but nothing in the job can ask the mirror what it already holds. A
# reference the cache already had reports a "miss" that is really a hit, which
# NARROWS the apparent gap — so a contaminated sample under-reports the channel
# and never invents one. Every sample is printed rather than only the summary,
# so a miss arm holding a hit-shaped outlier is visible to whoever reads it.
#
# THE VERDICT IS THE OVERLAP, NOT THE RATIO. Two medians an order of magnitude
# apart still leak nothing if the distributions overlap, because an attacker
# sees one sample and not a median. So the line that matters is whether the
# slowest hit is faster than the fastest miss.
#
# IT REPORTS AND DOES NOT FAIL. A timing threshold enforced in CI is a flake
# generator, and the row asks for a reading to sharpen guidance rather than a
# gate. The only failure is a reading that could not be taken at all.
#
# Usage:
#   scripts/e2e/mirror-timing.sh
#
# Environment:
#   REGISTRY_MIRRORS — <upstream>=<mirror> map (scripts/lib/registry-mirror.sh).
#                      Unset or empty means no mirror is wired — the hosted lane
#                      and a developer's `make e2e` — so the probe reports a skip
#                      and exits 0.
#   MIRROR_TIMING_HTTP_TIMEOUT — seconds any one fetch may take (default 60).
#
# Exit: 0 when the reading was taken or deliberately skipped, 1 when it could not
# be taken — an unreachable mirror, or a reference whose blob digest did not
# resolve. Never non-zero because of what the numbers said.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${REPO_ROOT}/scripts/lib/registry-mirror.sh"

HTTP_TIMEOUT="${MIRROR_TIMING_HTTP_TIMEOUT:-60}"

# Docker Hub, because every reference below is Hub's and dockerd's mirror
# redirect is irrelevant here — these are direct HTTP fetches against the mirror
# Service, not pulls.
PROBE_UPSTREAM="docker.io"

# The references, one pair of samples each. Small on purpose: the probe adds
# their layers to a cache the suite does not otherwise fill, and pays that
# upstream bandwidth once per run. Distinct tags rather than distinct
# repositories so the set stays inside two well-known images whose layers are
# small and whose availability is not in question.
PROBE_REFS=(
	"library/alpine:3.18"
	"library/alpine:3.19"
	"library/busybox:1.35"
	"library/busybox:1.36"
)

ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

# --- pure helpers (unit-tested; no curl, no network) -------------------------

# stats — read whitespace-separated milliseconds on stdin, print `min median max`.
# The median is the lower of the two middles on an even count, which is the
# convention that needs no interpolation and so cannot invent a value no sample
# held. Prints nothing for empty input, which the caller reads as an empty arm.
stats() {
	sort -n | awk '
		{ v[n++] = $1 }
		END {
			if (n == 0) exit
			printf "%d %d %d\n", v[0], v[int((n - 1) / 2)], v[n - 1]
		}'
}

# separation_verdict — read `hit <ms>` / `miss <ms>` lines on stdin and print the
# reading: each arm's min/median/max, and whether they overlap.
#
# Overlap is the whole question. An attacker times ONE fetch and asks which arm
# it came from, so two arms whose ranges touch leak nothing however far apart
# their medians are. `max(hit) < min(miss)` is therefore the line, and it is
# reported as a property of this sample rather than of the channel: a bigger
# sample can only close a gap this one showed, never open one it did not.
#
# Returns 1 when either arm is empty — a reading that was not taken, which must
# never read as a refuted channel.
separation_verdict() {
	local arm ms hits=() misses=()
	while read -r arm ms; do
		case "${arm}" in
		hit) hits+=("${ms}") ;;
		miss) misses+=("${ms}") ;;
		esac
	done
	if ((${#hits[@]} == 0 || ${#misses[@]} == 0)); then
		echo "REFUSE  the reading was not taken: ${#hits[@]} hit sample(s), ${#misses[@]} miss sample(s)"
		return 1
	fi
	local hit_stats miss_stats hmin hmed hmax mmin mmed mmax
	hit_stats="$(printf '%s\n' "${hits[@]}" | stats)"
	miss_stats="$(printf '%s\n' "${misses[@]}" | stats)"
	read -r hmin hmed hmax <<<"${hit_stats}"
	read -r mmin mmed mmax <<<"${miss_stats}"
	printf 'hit   n=%d  min=%sms  median=%sms  max=%sms\n' "${#hits[@]}" "${hmin}" "${hmed}" "${hmax}"
	printf 'miss  n=%d  min=%sms  median=%sms  max=%sms\n' "${#misses[@]}" "${mmin}" "${mmed}" "${mmax}"
	if ((hmax < mmin)); then
		printf 'SEPARATED  every hit (<=%sms) was faster than every miss (>=%sms) in this sample: one fetch is enough to tell them apart\n' \
			"${hmax}" "${mmin}"
	else
		printf 'OVERLAPPING  the slowest hit (%sms) is not faster than the fastest miss (%sms): a single fetch does not tell them apart in this sample\n' \
			"${hmax}" "${mmin}"
	fi
	return 0
}

# --- probes ------------------------------------------------------------------

# blob_digest ENDPOINT REPO REF — print the digest of the first layer of REF's
# linux/amd64 image, or nothing. Resolves an index to its platform manifest
# first, since a multi-arch tag's own document carries no layers.
#
# The manifest fetches are NOT timed and do not warm the blob: a pull-through
# cache revalidates a manifest upstream on every request and stores blob content
# separately, so resolving a digest leaves the blob exactly as cold as it was.
blob_digest() {
	local endpoint="$1" repo="$2" ref="$3" doc child
	doc="$(curl -s --max-time "${HTTP_TIMEOUT}" -H "Accept: ${ACCEPT}" \
		"http://${endpoint}/v2/${repo}/manifests/${ref}" || true)"
	child="$(jq -r '.manifests // [] | map(select(.platform.os == "linux" and .platform.architecture == "amd64")) | .[0].digest // empty' <<<"${doc}" 2>/dev/null || true)"
	if [[ -n "${child}" ]]; then
		doc="$(curl -s --max-time "${HTTP_TIMEOUT}" -H "Accept: ${ACCEPT}" \
			"http://${endpoint}/v2/${repo}/manifests/${child}" || true)"
	fi
	jq -r '.layers // [] | .[0].digest // empty' <<<"${doc}" 2>/dev/null || true
}

# fetch_ms ENDPOINT REPO DIGEST — print the milliseconds a blob GET took, or
# nothing when curl produced no timing. The body is discarded; what is measured
# is the whole response, which is what an attacker measures too.
fetch_ms() {
	local endpoint="$1" repo="$2" digest="$3" secs
	secs="$(curl -s -o /dev/null --max-time "${HTTP_TIMEOUT}" \
		-w '%{time_total}' "http://${endpoint}/v2/${repo}/blobs/${digest}" || true)"
	[[ -n "${secs}" ]] || return 0
	awk -v s="${secs}" 'BEGIN { printf "%d\n", s * 1000 }'
}

main() {
	local endpoint
	endpoint="$(mirror_for "${PROBE_UPSTREAM}")"
	if [[ -z "${endpoint}" ]]; then
		echo "mirror-timing: no mirror wired for ${PROBE_UPSTREAM} (REGISTRY_MIRRORS unset or without it) — skipping"
		return 0
	fi

	echo "mirror-timing: ${#PROBE_REFS[@]} references through ${endpoint}, one cold fetch and one warm fetch each"

	local -a repos=() digests=()
	local ref repo tag digest
	for ref in "${PROBE_REFS[@]}"; do
		repo="${ref%:*}"
		tag="${ref##*:}"
		digest="$(blob_digest "${endpoint}" "${repo}" "${tag}")"
		if [[ -z "${digest}" ]]; then
			echo "mirror-timing: no layer digest resolved for ${ref} — the mirror is unreachable or serving something else" >&2
			return 1
		fi
		repos+=("${repo}")
		digests+=("${digest}")
	done

	# Interleaved: reference i is fetched cold, then reference i-1 is fetched
	# again. Every hit is therefore one whole miss away from its own miss, and
	# the two arms are spread across the same stretch of wall clock.
	local samples="" i ms
	for ((i = 0; i < ${#repos[@]}; i++)); do
		ms="$(fetch_ms "${endpoint}" "${repos[i]}" "${digests[i]}")"
		[[ -z "${ms}" ]] || samples+="miss ${ms}"$'\n'
		printf '  miss  %-24s %sms\n' "${PROBE_REFS[i]}" "${ms:-timeout}"
		((i == 0)) && continue
		ms="$(fetch_ms "${endpoint}" "${repos[i - 1]}" "${digests[i - 1]}")"
		[[ -z "${ms}" ]] || samples+="hit ${ms}"$'\n'
		printf '  hit   %-24s %sms\n' "${PROBE_REFS[i - 1]}" "${ms:-timeout}"
	done
	# The last reference has no later iteration to be re-fetched in.
	i=$((${#repos[@]} - 1))
	ms="$(fetch_ms "${endpoint}" "${repos[i]}" "${digests[i]}")"
	[[ -z "${ms}" ]] || samples+="hit ${ms}"$'\n'
	printf '  hit   %-24s %sms\n' "${PROBE_REFS[i]}" "${ms:-timeout}"

	echo
	separation_verdict <<<"${samples}"
}

[[ -n "${MIRROR_TIMING_LIB_ONLY:-}" ]] || main "$@"
