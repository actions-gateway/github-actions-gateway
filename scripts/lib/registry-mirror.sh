# Registry-mirror ref rewriting, shared by the clients Q408 Phase 3 wires: the
# docker pulls (fetch/pull-image-with-retry.sh), helm's OCI client
# (e2e/chart-released-upgrade-check.sh) and buildkit's config
# (e2e/buildkitd-mirror-config.sh). Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/lib/registry-mirror.sh
#   source "$REPO_ROOT/scripts/lib/registry-mirror.sh"
#
# The map is one environment variable, REGISTRY_MIRRORS, holding
# <upstream>=<mirror endpoint> pairs separated by whitespace or commas:
#
#   REGISTRY_MIRRORS="ghcr.io=mirror-ghcr-io.gag-registry-mirror.svc.cluster.local:5000 \
#                     quay.io=mirror-quay-io.gag-registry-mirror.svc.cluster.local:5000"
#
# Unset or empty means no rewriting, which is what every caller outside the
# dogfood e2e tenant sees: the hosted lane, a developer's `make e2e`, and
# publish.yml all pull direct. The dogfood Kata tenant sets it on the worker
# from deploy/dogfood-e2e/overlays/kata (the registry-mirror-wiring ConfigMap),
# so the address of a cluster-local Service never enters this repo's workflows.
#
# docker.io is IN that map, alongside the four hosts whose clients have no
# mirror setting of their own. The inner dockerd also carries `registry-mirrors`
# in the same ConfigMap's daemon.json and would serve every Hub pull without
# help — but buildkit reads neither that nor a rewritten docker ref, and the
# Dockerfile's `golang` and `docker:29-cli` stages are Hub's. One map covering
# all five is what keeps the buildkit config complete. Rewriting a Hub ref is
# therefore redundant rather than wrong, and the implicit forms of Hub's
# grammar — a bare `alpine`, an implicit `library/` namespace — are resolved
# below so that redundancy stays correct.
# shellcheck shell=bash

# mirror_for HOST — print the mirror endpoint configured for registry HOST, or
# nothing when there is none. Pure: reads REGISTRY_MIRRORS only.
mirror_for() {
	local host="$1" entry map="${REGISTRY_MIRRORS:-}"
	# Deliberately unquoted: the map is whitespace/comma separated and word
	# splitting is how it is parsed.
	# shellcheck disable=SC2086
	for entry in ${map//,/ }; do
		[[ "${entry%%=*}" == "${host}" ]] || continue
		printf '%s' "${entry#*=}"
		return 0
	done
	return 0
}

# mirror_ref_host REF — print the registry host REF names, resolving the two
# implicit forms docker's grammar allows: a first component with no `.`, no `:`
# and not `localhost` is not a host at all but the start of a Docker Hub
# repository path, so `alpine` and `kindest/node` are both docker.io.
mirror_ref_host() {
	local ref="$1" first="${1%%/*}"
	if [[ "${ref}" == */* && ( "${first}" == *.* || "${first}" == *:* || "${first}" == "localhost" ) ]]; then
		printf '%s' "${first}"
	else
		printf 'docker.io'
	fi
}

# mirror_rewrite REF — print REF with its registry host replaced by the
# configured mirror, or REF unchanged when no mirror applies. Pure.
#
# A digest-bearing ref is returned UNCHANGED, and says so on stderr. `docker
# pull name:tag@digest` stores the image under `name@digest`, and a digest is
# not a legal `docker tag` target, so a rewritten pull could not restore the
# local reference the caller asked for — the rewrite would succeed and the next
# `kind load` of the original ref would fail. Such a ref is not thereby off the
# mirror: every digest-pinned ref in the measured job-time inventory (plan §2.2)
# belongs to a client that carries its own mirror setting, dockerd's daemon.json
# or buildkit's config, which redirects it without touching the name. The
# warning is for the case that leaves — a digest-pinned ref pulled by a client
# with no mirror setting, which under the Phase 4 policy fails at pull time.
mirror_rewrite() {
	local ref="$1" host mirror rest
	host="$(mirror_ref_host "${ref}")"
	mirror="$(mirror_for "${host}")"
	[[ -n "${mirror}" ]] || { printf '%s' "${ref}"; return 0; }

	if [[ "${ref}" == *@* ]]; then
		echo "registry-mirror: ${ref} is digest-pinned, so the ref is not rewritten;" >&2
		echo "                 its client's own mirror setting has to serve it." >&2
		printf '%s' "${ref}"
		return 0
	fi

	if [[ "${ref}" == "${host}/"* ]]; then
		rest="${ref#"${host}/"}"
	else
		# An implicit Docker Hub name. A single-segment repository lives under
		# the implicit `library/` namespace, which only Hub's own resolution
		# supplies — a mirror addressed by hostname does not.
		rest="${ref}"
		[[ "${rest}" == */* ]] || rest="library/${rest}"
	fi
	printf '%s/%s' "${mirror}" "${rest}"
}

# mirror_hosts — print each configured upstream host, one per line, in the order
# REGISTRY_MIRRORS declares them. Pure.
mirror_hosts() {
	local entry map="${REGISTRY_MIRRORS:-}"
	# shellcheck disable=SC2086
	for entry in ${map//,/ }; do
		printf '%s\n' "${entry%%=*}"
	done
}
