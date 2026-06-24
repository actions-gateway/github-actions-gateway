#!/usr/bin/env bash
#
# Reproducible preview/screenshot harness for the github-actions-gateway
# monitoring artifacts (Q186). Spins up a throwaway kind cluster, installs the
# public kube-prometheus-stack Helm chart (Prometheus Operator + Prometheus +
# Grafana + image-renderer + kube-state-metrics), applies the *real* artifacts
# from the parent directory (../prometheusrule.yaml, ../grafana-dashboard.json),
# feeds Prometheus a synthetic actions_gateway_* metrics stream, and renders the
# dashboard to a PNG via Grafana's image renderer.
#
# Re-run it whenever the dashboard JSON or the rules change to get a fresh
# screenshot. Nothing here is applied to a real cluster and nothing is committed
# except this harness itself.
#
# Usage:
#   ./render.sh            # create cluster + stack, apply artifacts, render PNG
#   ./render.sh shot       # re-apply artifacts + re-render only (fast iteration)
#   ./render.sh down       # delete the throwaway cluster
#
# Knobs (environment variables, with defaults):
#   CLUSTER=gag-obs  RELEASE=kps  MON_NS=monitoring
#   OUT=./actions-gateway-dashboard.png
#   WAIT=180         # seconds to let counters/histograms accumulate before render
#   WIDTH=1500  HEIGHT=2300  FROM=now-20m  TO=now
#
# Prerequisites: docker, kind, helm, kubectl, curl on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MON_DIR="$(dirname "$SCRIPT_DIR")"
readonly SCRIPT_DIR MON_DIR

CLUSTER="${CLUSTER:-gag-obs}"
RELEASE="${RELEASE:-kps}"
MON_NS="${MON_NS:-monitoring}"
OUT="${OUT:-./actions-gateway-dashboard.png}"
WAIT="${WAIT:-180}"
WIDTH="${WIDTH:-1500}"
HEIGHT="${HEIGHT:-2300}"
FROM="${FROM:-now-20m}"
TO="${TO:-now}"
readonly CHART="prometheus-community/kube-prometheus-stack"
readonly DASH_UID="actions-gateway"

PF_PID=""

cleanup() {
	if [[ -n "$PF_PID" ]]; then
		kill "$PF_PID" 2>/dev/null || true
		PF_PID=""
	fi
}
trap cleanup EXIT

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() {
	printf '\033[1;31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

require_cmds() {
	# kubectl on macOS often ships only under Docker.app — add it if missing.
	if ! command -v kubectl >/dev/null 2>&1; then
		local docker_bin="/Applications/Docker.app/Contents/Resources/bin"
		[[ -d "$docker_bin" ]] && PATH="$PATH:$docker_bin"
	fi
	local cmd
	for cmd in docker kind helm kubectl curl; do
		command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
	done
}

# port_forward <svc> <local:remote> — start a background port-forward, record
# its PID, and wait until the local port answers.
port_forward() {
	local svc="$1" ports="$2" local_port="${2%%:*}"
	cleanup
	kubectl -n "$MON_NS" port-forward "svc/$svc" "$ports" >/dev/null 2>&1 &
	PF_PID="$!"
	local attempt
	for attempt in $(seq 1 30); do
		if curl -fsS "http://localhost:$local_port" >/dev/null 2>&1; then
			return 0
		fi
		(( attempt > 0 )) && sleep 1
	done
	die "port-forward to $svc did not become ready"
}

ensure_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
		log "reusing kind cluster '$CLUSTER'"
	else
		log "creating kind cluster '$CLUSTER'"
		kind create cluster --name "$CLUSTER" --wait 120s
	fi
	kubectl config use-context "kind-$CLUSTER" >/dev/null
}

install_stack() {
	log "installing kube-prometheus-stack (release '$RELEASE')"
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
	helm repo update prometheus-community >/dev/null
	helm upgrade --install "$RELEASE" "$CHART" \
		-n "$MON_NS" --create-namespace \
		-f "$SCRIPT_DIR/values.yaml" \
		--wait --timeout 10m
}

apply_artifacts() {
	log "applying synthetic workload + the real monitoring artifacts"
	kubectl apply -f "$SCRIPT_DIR/workload.yaml"
	kubectl create configmap ag-exporter-code -n team-a \
		--from-file=exporter.py="$SCRIPT_DIR/exporter.py" \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl -n team-a rollout restart deploy/ag-metrics-exporter

	# The real PrometheusRule artifact.
	kubectl apply -n "$MON_NS" -f "$MON_DIR/prometheusrule.yaml"

	# The real dashboard artifact, imported via the Grafana sidecar.
	kubectl create configmap ag-dashboard -n "$MON_NS" \
		--from-file=actions-gateway.json="$MON_DIR/grafana-dashboard.json" \
		--dry-run=client -o yaml |
		kubectl label --local -f - grafana_dashboard=1 -o yaml |
		kubectl apply -f -

	kubectl -n team-a rollout status deploy/ag-metrics-exporter --timeout=120s
}

render() {
	log "letting metrics accumulate (${WAIT}s) so rate()/histograms have data"
	sleep "$WAIT"
	log "rendering dashboard to $OUT"
	port_forward "$RELEASE-grafana" "3000:80"
	local code
	code="$(curl -s -u admin:admin -o "$OUT" -w '%{http_code}' \
		"http://localhost:3000/render/d/$DASH_UID/$DASH_UID?orgId=1&from=$FROM&to=$TO&width=$WIDTH&height=$HEIGHT&theme=dark&kiosk=1")"
	cleanup
	[[ "$code" == "200" ]] || die "Grafana render returned HTTP $code"
	log "wrote $OUT ($(wc -c <"$OUT" | tr -d ' ') bytes)"
}

down() {
	require_cmds
	log "deleting kind cluster '$CLUSTER'"
	kind delete cluster --name "$CLUSTER"
}

main() {
	local action="${1:-up}"
	case "$action" in
	up)
		require_cmds
		ensure_cluster
		install_stack
		apply_artifacts
		render
		;;
	shot)
		require_cmds
		kubectl config use-context "kind-$CLUSTER" >/dev/null
		apply_artifacts
		render
		;;
	down)
		down
		;;
	*)
		die "unknown action '$action' (expected: up | shot | down)"
		;;
	esac
}

main "$@"
