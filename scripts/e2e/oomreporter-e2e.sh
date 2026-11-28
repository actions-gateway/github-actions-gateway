#!/usr/bin/env bash
#
# oomreporter-e2e.sh — the oom-reporter's kind e2e lane: one kill at each limit, and the
# condition and the Event the reporter writes for it (Q1403).
# The chart's kernelLog option is on: each kill's line reaches the agent's report, and a line a
# job forged is refused.
#
# Makes its own kind cluster, a control plane and one worker, so that nothing else's pods
# share the worker's limits. The worker reserves all but 1 GiB of the host's memory for the
# system, which leaves its pods about 1 GiB to share: a pod that takes 2 GiB is killed
# at that limit and not at the host's. The kill above the pods is a limit the test sets on
# the worker's cgroup above them and takes off again.
#
# Builds the reporter's image and the hog's, loads them, installs charts/oom-reporter as an
# operator would, runs cmd/oomreporter's e2e test, and deletes the cluster however it ended.
# OOM_E2E_KEEP=1 leaves the cluster for a look; delete it with `kind delete cluster --name`.
#
# Needs docker, kind, kubectl, helm and go. Backs `make e2e-oomreporter`.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd docker https://docs.docker.com/get-docker/
require_cmd kind https://kind.sigs.k8s.io/docs/user/quick-start/#installation
require_cmd kubectl https://kubernetes.io/docs/tasks/tools/
require_cmd helm https://helm.sh/docs/intro/install/
require_cmd go https://go.dev/doc/install

CLUSTER="${OOM_E2E_CLUSTER:-oom-reporter-e2e}"
NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.35.5}"
WORK="${REPO_ROOT}/tmp/oomreporter-e2e"
KUBECONFIG_FILE="${WORK}/kubeconfig"
REPORTER_IMAGE="oomreporter:e2e"
HOG_IMAGE="oom-hog:e2e"
RELEASE_NS="oom-reporter"
TENANT_NS="oom-e2e"

cleanup() {
  local rc=$?
  if (( rc != 0 )) && [[ -f "$KUBECONFIG_FILE" ]]; then
    echo "==> the reporter's logs, for the failure"
    kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$RELEASE_NS" logs -l app.kubernetes.io/name=oom-reporter \
      --all-containers --prefix --tail=200 || true
    kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$TENANT_NS" get pods,events -o wide || true
    echo "==> the agents' reports"
    kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$RELEASE_NS" get configmaps -o yaml || true
  fi
  if [[ "${OOM_E2E_KEEP:-}" == "1" ]]; then
    echo "==> keeping cluster ${CLUSTER} (OOM_E2E_KEEP=1)"
  else
    kind delete cluster --name "$CLUSTER" || true
  fi
  exit "$rc"
}

mkdir -p "$WORK/hog"
# A kubeconfig left by an earlier run names a cluster this one has not made.
rm -f "$KUBECONFIG_FILE"
if kind get clusters | grep -qx "$CLUSTER"; then
  die "a cluster named ${CLUSTER} exists already; delete it, or set OOM_E2E_CLUSTER"
fi

# kind's nodes see the host's memory as their own, so what the worker keeps for its pods is
# the host's less what it reserves.
total_mib=$(( $(docker info --format '{{.MemTotal}}') >> 20 ))
reserved_mib=$(( total_mib - 1024 ))
(( reserved_mib > 0 )) || die "the host has ${total_mib} MiB, too little to leave the worker's pods 1 GiB"
cat > "$WORK/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    kubeadmConfigPatches:
      - |
        kind: JoinConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            system-reserved: memory=${reserved_mib}Mi
EOF

trap cleanup EXIT
echo "==> kind cluster ${CLUSTER} (${NODE_IMAGE}), the worker reserving ${reserved_mib} of ${total_mib} MiB"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$WORK/kind.yaml" \
  --kubeconfig "$KUBECONFIG_FILE" --wait 180s
k() { kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"; }

step "images"
docker build --target oomreporter -t "$REPORTER_IMAGE" .
(cd cmd/oomreporter && CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$WORK/hog/hog" ./test/hog)
printf 'FROM scratch\nCOPY hog /hog\nENTRYPOINT ["/hog"]\n' > "$WORK/hog/Dockerfile"
docker build -t "$HOG_IMAGE" "$WORK/hog"
kind load docker-image --name "$CLUSTER" "$REPORTER_IMAGE" "$HOG_IMAGE"

step "the chart, as an operator installs it"
k create namespace "$RELEASE_NS"
k label namespace "$RELEASE_NS" pod-security.kubernetes.io/enforce=privileged
k create namespace "$TENANT_NS"
k label node "${CLUSTER}-control-plane" node-restriction.kubernetes.io/oom-reporter=controller
helm install oom-reporter charts/oom-reporter --kubeconfig "$KUBECONFIG_FILE" \
  --namespace "$RELEASE_NS" --rollback-on-failure --timeout 5m \
  --set "namespaces={${TENANT_NS}}" \
  --set allowFloatingImageTags=true \
  --set image.repository="${REPORTER_IMAGE%%:*}" --set image.tag="${REPORTER_IMAGE##*:}" \
  --set agent.podsRoot=kubelet.slice/kubelet-kubepods.slice \
  --set kernelLog.enabled=true \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule'

step "the kills"
(cd cmd/oomreporter && OOM_E2E_KUBECONFIG="$KUBECONFIG_FILE" OOM_E2E_WORKER="${CLUSTER}-worker" \
  OOM_E2E_NAMESPACE="$TENANT_NS" OOM_E2E_RELEASE_NAMESPACE="$RELEASE_NS" OOM_E2E_HOG_IMAGE="docker.io/library/${HOG_IMAGE}" \
  go test -tags e2e -run '^TestE2E_' -count=1 -timeout 20m -v .)
