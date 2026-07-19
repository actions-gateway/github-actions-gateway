# Shared Kata Containers install helpers for the dogfood e2e pool. Source, don't
# execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/lib/common.sh
#   source "$REPO_ROOT/scripts/lib/common.sh"
#   # shellcheck source=scripts/dogfood/lib/kata.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/kata.sh"
#
# Sourced by both scripts/dogfood/e2e-setup.sh (the one-time infra bootstrap)
# and scripts/dogfood/ops.sh (kata-install — re-install/upgrade the DaemonSet
# without re-running the whole billable setup). Keeping the install in one place
# means the ~30 lines of load-bearing helm flags below are reviewed once, not
# re-derived per invocation. Callers must set REPO_ROOT, have `set -euo pipefail`
# active, and point kubectl at the dogfood cluster first (the functions run
# against the active context — gate them with gke_get_credentials_and_verify).
# shellcheck shell=bash

# Kata is installed from upstream's OCI Helm chart. Kata no longer publishes the
# kata-deploy-stable.yaml / kata-rbac.yaml release assets this used to fetch —
# those URLs now return HTTP 404 (verified under Q226). Pin a released version:
# https://github.com/kata-containers/kata-containers/releases
KATA_VERSION="${KATA_VERSION:-3.32.0}"
KATA_CHART="oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy"
# Label that scopes the kata-deploy installer to the e2e pool. Distinct from
# katacontainers.io/kata-runtime (which kata-deploy sets once the runtime is
# installed, and the RuntimeClass schedules on): kata-deploy must target nodes
# where the runtime does NOT exist yet.
KATA_POOL_LABEL_KEY="gag.dev/kata-ci"
# The runtime-ready label is ALSO baked into the pool config (e2e-setup.sh's
# create_node_pool), not because the runtime exists at boot — it doesn't — but
# because cluster-autoscaler scale-from-zero simulates against the pool's
# configured labels only; without it no Kata pod can ever trigger the 0→N
# scale-up (found live under Q286). Same pattern GKE uses for its gVisor sandbox
# pools. Referenced by e2e-setup.sh's create_node_pool, not here, so shellcheck
# (which checks files independently) sees it as unused in this file.
# shellcheck disable=SC2034  # consumed by the sourcing script (e2e-setup.sh)
KATA_RUNTIME_LABEL_KEY="katacontainers.io/kata-runtime"

# kata_install — install (or upgrade) the kata-deploy DaemonSet on the e2e pool
# via Helm. Idempotent: `helm upgrade --install` converges to the pinned chart
# version whether or not it is already present.
kata_install() {
	require_cmd helm "https://helm.sh/docs/intro/install/"

	# Scope the installer to the e2e pool via the pool's own label. The chart
	# creates the kata-qemu RuntimeClass (with the correct pod overhead); we add
	# the `kata` alias separately in kata_apply_runtimeclass.
	echo "Installing kata-deploy ${KATA_VERSION} via Helm (scoped to the e2e pool)..."
	# The tolerations are load-bearing: the e2e pool is tainted
	# dedicated=e2e:NoSchedule and the chart ships none by default, so without
	# them the installer can never reach the only nodes it targets (found live
	# under Q286). --set-string on the label value: nodeSelector values must be
	# strings, and a bare --set types `true` as a boolean, failing the chart's
	# server-side apply (Q286).
	helm upgrade --install kata-deploy "${KATA_CHART}" \
		--version "${KATA_VERSION}" \
		--namespace kube-system \
		--set-string "nodeSelector.${KATA_POOL_LABEL_KEY//./\\.}=true" \
		--set-string "tolerations[0].key=dedicated" \
		--set-string "tolerations[0].value=e2e" \
		--set-string "tolerations[0].effect=NoSchedule" \
		--set-string "tolerations[0].operator=Equal" \
		--set shims.disableAll=true \
		--set shims.qemu.enabled=true \
		--set defaultShim.amd64=qemu \
		--set monitor.enabled=false \
		--set runtimeClasses.enabled=true \
		--set runtimeClasses.createDefault=false

	echo "Waiting for Kata DaemonSet (only completes once e2e nodes exist)..."
	echo "  (With 0 e2e nodes running, this may print a warning and continue.)"
	kubectl rollout status daemonset/kata-deploy -n kube-system --timeout=2m || true
}

# kata_apply_runtimeclass — create the `kata` RuntimeClass alias over the
# chart-owned kata-qemu handler. Idempotent server-side upsert.
kata_apply_runtimeclass() {
	# The `kata` alias -> kata-qemu handler, so the hypervisor can be retargeted
	# without editing every pod spec. The chart already owns `kata-qemu` itself.
	#
	# NOTE: `scheduling` accepts ONLY `nodeSelector` and `tolerations`. An earlier
	# revision nested them under `scheduling.nodeClassification`, which the API
	# server rejects outright ("strict decoding error: unknown field
	# scheduling.nodeClassification") — caught under Q226.
	#
	# nodeSelector pins Kata pods to nodes where kata-deploy has FINISHED
	# installing (it applies katacontainers.io/kata-runtime=true on completion).
	# No tolerations here: the tenant's podTemplate already tolerates
	# dedicated=e2e:NoSchedule, and this alias must stay identical to the one in
	# deploy/kata-ci/runtimeclass.yaml.
	echo "Applying kata RuntimeClass alias..."
	kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata
handler: kata-qemu
overhead:
  podFixed:
    memory: "160Mi"
    cpu: "250m"
scheduling:
  nodeSelector:
    katacontainers.io/kata-runtime: "true"
EOF
}
