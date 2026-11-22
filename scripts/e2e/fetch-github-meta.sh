#!/usr/bin/env bash
#
# fetch-github-meta.sh — fetch GitHub's published IP ranges (GET /meta) with the
# job's token and write them to a file, so the e2e suite can seed the GMC's
# persisted IP-range ConfigMap before the GMC starts (Q1119).
#
# An unauthenticated /meta call spends the runner address's quota of 60 requests
# an hour, which every other tenant of that address spends too. Once it is gone
# the GMC's own fetch answers 403, the proxy NetworkPolicy keeps no 443 rule, and
# both [real-github-egress] specs see `CONNECT tunnel failed, response 502`. A
# token-authenticated call draws on the token's quota instead.
#
# Usage:
#   GH_TOKEN=... scripts/e2e/fetch-github-meta.sh <out-file>
#
# Environment:
#   GH_TOKEN        token for the Authorization header. Required: without it the
#                   call is the unauthenticated one this script exists to avoid.
#                   It reaches curl through a config file descriptor, never argv,
#                   and is never printed.
#   GITHUB_API_URL  API origin (default https://api.github.com).
#
# Exit status: 0 with <out-file> written only when the response was HTTP 200 and
# carried non-empty `api`, `actions` and `web` range lists; non-zero otherwise,
# leaving no <out-file>. A caller that treats failure as "no seed" runs the suite
# as it did before this script existed.
set -euo pipefail
shopt -s inherit_errexit

readonly FETCH_TIMEOUT_SECONDS=30

# Global so the EXIT trap, which runs after main returns, can still read it.
tmp=""

die() {
	printf 'fetch-github-meta: %s\n' "$*" >&2
	exit 1
}

main() {
	local out="${1:-}"
	[[ -n "${out}" ]] || die "usage: fetch-github-meta.sh <out-file>"
	[[ -n "${GH_TOKEN:-}" ]] || die "GH_TOKEN is not set; refusing the unauthenticated call"

	local api="${GITHUB_API_URL:-https://api.github.com}"
	local status
	mkdir -p "$(dirname "${out}")"
	tmp="$(mktemp "${out}.XXXXXX")"
	trap 'rm -f "${tmp}"' EXIT

	status="$(curl --silent --show-error --max-time "${FETCH_TIMEOUT_SECONDS}" \
		--header 'Accept: application/json' \
		--config <(printf 'header = "Authorization: Bearer %s"\n' "${GH_TOKEN}") \
		--output "${tmp}" --write-out '%{http_code}' "${api}/meta")" \
		|| die "request to ${api}/meta failed"
	[[ "${status}" == "200" ]] || die "${api}/meta answered HTTP ${status}"

	jq -e '(.api | length > 0) and (.actions | length > 0) and (.web | length > 0)' "${tmp}" >/dev/null \
		|| die "response has no api, actions or web ranges"

	mv "${tmp}" "${out}"
	printf 'fetch-github-meta: wrote %s (%s api, %s actions, %s web ranges)\n' "${out}" \
		"$(jq '.api | length' "${out}")" "$(jq '.actions | length' "${out}")" "$(jq '.web | length' "${out}")"
}

main "$@"
