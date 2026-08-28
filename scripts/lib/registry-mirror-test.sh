#!/usr/bin/env bash
#
# Unit tests for scripts/lib/registry-mirror.sh — the ref rewriting Q408 Phase 3
# points the e2e job's image clients at the in-cluster pull-through caches with.
#
# Why it is tested. Three consumers share this map (the docker pulls, helm's OCI
# chart pull, buildkit's generated config), and both directions of a mistake here
# are quiet: a rewrite that does not happen leaves the client talking to its
# upstream, which is green until Phase 4 deletes `e2e-open-egress` and then fails
# a booked dogfood session; a rewrite that mangles the ref fails the run at pull
# time with an error naming a host nobody wrote down. So both are asserted, and
# so is the no-map case — the hosted lane, publish.yml and a developer's `make
# e2e` all run with REGISTRY_MIRRORS unset and must be untouched by any of it.
#
# Docker's reference grammar is where the traps are. A first component with no
# dot and no colon is a repository path, not a host, so `alpine` and
# `kindest/node` are both Docker Hub; and a single-segment Hub name carries an
# implicit `library/` namespace that only Hub's own resolution supplies — a
# mirror addressed by hostname serves `library/alpine`, never `alpine`.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${REPO_ROOT}/scripts/lib/registry-mirror.sh"

fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

# The map the dogfood Kata tenant supplies, verbatim in shape:
# deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml.
NS=gag-registry-mirror.svc.cluster.local:5000
FULL_MAP="docker.io=mirror-docker-io.${NS} ghcr.io=mirror-ghcr-io.${NS}"
FULL_MAP+=" quay.io=mirror-quay-io.${NS} registry.k8s.io=mirror-registry-k8s-io.${NS}"
FULL_MAP+=" gcr.io=mirror-gcr-io.${NS}"

# --- no map: every caller outside the tenant ---------------------------------

unset REGISTRY_MIRRORS
check 'an unset map rewrites nothing' \
	'quay.io/jetstack/cert-manager-controller:v1.20.2' \
	"$(mirror_rewrite quay.io/jetstack/cert-manager-controller:v1.20.2)"
check 'an unset map resolves no mirror' '' "$(mirror_for quay.io)"
check 'an unset map lists no hosts' '' "$(mirror_hosts)"

REGISTRY_MIRRORS=''
check 'an empty map rewrites nothing' 'alpine:3' "$(mirror_rewrite alpine:3)"

# --- host resolution ---------------------------------------------------------

REGISTRY_MIRRORS="${FULL_MAP}"
check 'an explicit host is read as the host' 'quay.io' "$(mirror_ref_host quay.io/jetstack/x:1)"
check 'a two-segment Hub name is not a host' 'docker.io' "$(mirror_ref_host kindest/node:v1.35.5)"
check 'a bare Hub name is not a host' 'docker.io' "$(mirror_ref_host alpine:3)"
check 'a host carrying a port is a host' 'localhost:5000' "$(mirror_ref_host localhost:5000/img:1)"
check 'localhost with no port is a host' 'localhost' "$(mirror_ref_host localhost/img:1)"

# --- the rewrites the measured inventory actually needs ----------------------
#
# One per non-Hub client of plan §2.2, taken from that table rather than
# invented, so a rewrite that works only on the shapes a test author thought of
# fails here rather than on the cluster.

check 'the released GMC image (ghcr.io)' \
	"mirror-ghcr-io.${NS}/actions-gateway/gmc:v1.2.0" \
	"$(mirror_rewrite ghcr.io/actions-gateway/gmc:v1.2.0)"
check 'a cert-manager image (quay.io)' \
	"mirror-quay-io.${NS}/jetstack/cert-manager-controller:v1.20.2" \
	"$(mirror_rewrite quay.io/jetstack/cert-manager-controller:v1.20.2)"
check 'the metrics-server image (registry.k8s.io, multi-segment repository)' \
	"mirror-registry-k8s-io.${NS}/metrics-server/metrics-server:v0.8.1" \
	"$(mirror_rewrite registry.k8s.io/metrics-server/metrics-server:v0.8.1)"
check 'the OCI chart repository (ghcr.io, three segments)' \
	"mirror-ghcr-io.${NS}/actions-gateway/charts/actions-gateway" \
	"$(mirror_rewrite ghcr.io/actions-gateway/charts/actions-gateway)"

# Hub, both implicit forms. The rewrite is redundant with dockerd's own
# registry-mirrors, so what matters is that it is not WRONG: a mirror serves
# `library/alpine`, and a client asking it for `alpine` gets a 404.
check 'a bare Hub name gains the implicit library/ namespace' \
	"mirror-docker-io.${NS}/library/alpine:3" "$(mirror_rewrite alpine:3)"
check 'a two-segment Hub name does not gain it' \
	"mirror-docker-io.${NS}/curlimages/curl:8.10.1" "$(mirror_rewrite curlimages/curl:8.10.1)"
check 'an explicitly-qualified Hub name loses only the host' \
	"mirror-docker-io.${NS}/library/registry:2" "$(mirror_rewrite docker.io/library/registry:2)"

# --- what is deliberately left alone -----------------------------------------

check 'an unmapped registry is untouched' \
	'registry.example.com/img:1' "$(mirror_rewrite registry.example.com/img:1)"
check 'the in-job local registry is untouched' \
	'127.0.0.1:5000/gmc:dev' "$(mirror_rewrite 127.0.0.1:5000/gmc:dev)"

# A digest-bearing ref: `docker pull name:tag@digest` stores the image under
# `name@digest`, and a digest is not a legal `docker tag` target, so a rewritten
# pull could not restore the local name the caller asked for. Left alone, and
# said out loud — silence here would look exactly like a rewrite that worked.
check 'a digest-pinned ref is not rewritten' \
	'docker.io/kindest/node:v1.35.5@sha256:ce97' \
	"$(mirror_rewrite docker.io/kindest/node:v1.35.5@sha256:ce97 2>/dev/null)"
if [[ "$(mirror_rewrite gcr.io/distroless/static:nonroot@sha256:d29e 2>&1 >/dev/null)" == *"digest-pinned"* ]]; then
	echo 'ok   a digest-pinned ref says so on stderr'
else
	echo 'FAIL a digest-pinned ref says so on stderr' >&2
	fails=$((fails + 1))
fi

# --- map parsing -------------------------------------------------------------

check 'the host list is the declared order' \
	'docker.io ghcr.io quay.io registry.k8s.io gcr.io' "$(mirror_hosts | tr '\n' ' ' | sed 's/ $//')"

REGISTRY_MIRRORS="quay.io=a:5000,ghcr.io=b:5000"
check 'commas separate entries as well as whitespace' 'b:5000' "$(mirror_for ghcr.io)"

# A host that is a prefix of another must not match it: `gcr.io` and
# `registry.k8s.io` are unrelated, but so are `gcr.io` and `mygcr.io`.
REGISTRY_MIRRORS="gcr.io=right:5000 mygcr.io=wrong:5000"
check 'host matching is exact, not a suffix' 'right:5000' "$(mirror_for gcr.io)"
check 'and the longer host is its own entry' 'wrong:5000' "$(mirror_for mygcr.io)"
check 'an unmapped host resolves to nothing' '' "$(mirror_for quay.io)"

if ((fails)); then
	echo "${fails} assertion(s) failed" >&2
	exit 1
fi
echo
echo "all registry-mirror.sh assertions passed"
