#!/usr/bin/env bash
#
# Reproducible preview/screenshot harness for the github-actions-gateway
# monitoring artifacts (Q186). Spins up a throwaway kind cluster, installs the
# public kube-prometheus-stack Helm chart (Prometheus Operator + Prometheus +
# Grafana + image-renderer + kube-state-metrics), applies the *real* artifacts
# from the parent directory (../prometheusrule.yaml, ../grafana-dashboard-*.json),
# feeds Prometheus a synthetic actions_gateway_* metrics stream, and renders the
# tenant + platform dashboards to PNGs via Grafana's image renderer.
#
# Re-run it whenever a dashboard JSON or the rules change to get fresh
# screenshots. Nothing here is applied to a real cluster and nothing is committed
# except this harness itself.
#
# Usage:
#   ./render.sh            # create cluster + stack, apply artifacts, render PNGs
#   ./render.sh shot       # re-apply artifacts + re-render only (fast iteration)
#   ./render.sh down       # delete the throwaway cluster
#
# Knobs (environment variables, with defaults):
#   CLUSTER=gag-obs  RELEASE=kps  MON_NS=monitoring
#   OUT_DIR=.        # directory the per-dashboard PNGs are written to
#   WAIT=660         # seconds to let counters/histograms accumulate before render
#   WIDTH=1500  HEIGHT=2300  FROM=now-10m  TO=now
# Runs against one CLUSTER are serialized against each other; see "concurrency"
# below. Set CLUSTER to opt out into a cluster of your own.
# WAIT and FROM are matched on purpose: the render window (FROM..TO) must fit
# inside the accumulated data or the time-series panels render mostly empty with
# a spike at the right edge. Keep WAIT >= the FROM window when changing either.
#
# Prerequisites: docker, kind, helm, kubectl, curl on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MON_DIR="$(dirname "$SCRIPT_DIR")"
readonly SCRIPT_DIR MON_DIR

CLUSTER="${CLUSTER:-gag-obs}"
RELEASE="${RELEASE:-kps}"
MON_NS="${MON_NS:-monitoring}"
OUT_DIR="${OUT_DIR:-.}"
WAIT="${WAIT:-660}"
WIDTH="${WIDTH:-1500}"
HEIGHT="${HEIGHT:-2300}"
FROM="${FROM:-now-10m}"
TO="${TO:-now}"
readonly CHART="prometheus-community/kube-prometheus-stack"
# Dashboard uid -> output PNG basename. Keep in sync with the uids in the
# ../grafana-dashboard-*.json files.
readonly DASH_UIDS=("actions-gateway-tenant" "actions-gateway-platform" "actions-gateway-budget" "actions-gateway-security")

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

# --- concurrency -------------------------------------------------------------
#
# Every run applies ../grafana-dashboard-*.json from *its own* worktree into the
# one shared cluster, and the whole WAIT window sits between that apply and the
# render. So two overlapping runs do not merely collide on Helm ("another
# operation (install/upgrade/rollback) is in progress"). They render each
# other's dashboards, and the harm lands on whichever session applied *first*:
# it gets a plausible PNG of a different branch's JSON, and the session that
# applied last is unaffected and cannot tell it just corrupted a peer (Q1072).
#
# Serializing on the cluster name keeps the single kube-prometheus-stack install
# this box can afford and makes the second session wait instead of fail. It also
# composes with the other way out: a session that sets CLUSTER gets its own
# cluster *and* its own lock, so it never queues behind the shared one.
#
# perl's flock, not flock(1) (absent on macOS) and not a mkdir lockdir: the
# kernel drops it when the holder dies, so a Ctrl-C'd render, routine on a
# harness that holds the lock for WAIT plus an install, never strands a lock
# that wedges every later run. Same mechanism, and the same reasons, as
# serialize_heavy_build in scripts/lib/common.sh.

# lock_path — the host-wide lock file for $CLUSTER, or nothing if this platform
# has no cache dir to put one in. Not repo-relative on purpose: sessions
# contending for the cluster are in different worktrees.
lock_path() {
	local base
	case "$(uname -s)" in
	Darwin) base="$HOME/Library/Caches" ;;
	Linux) base="${XDG_CACHE_HOME:-$HOME/.cache}" ;;
	*) return 0 ;;
	esac
	local dir="$base/github-actions-gateway"
	mkdir -p "$dir" 2>/dev/null || return 0
	# $CLUSTER lands in a path component; kind names are DNS labels, but keep a
	# stray separator from silently placing the lock somewhere nobody contends.
	printf '%s/preview-render.%s.lock\n' "$dir" "${CLUSTER//[^A-Za-z0-9._-]/_}"
}

# serialize_on_cluster — re-exec this script holding an exclusive lock on
# $CLUSTER, and hold it for the whole run. Pass the script's own "$@".
serialize_on_cluster() {
	# Re-entry guard, and cluster-blind on purpose: a locked run re-invoking this
	# script for a *different* CLUSTER would skip the lock. Nothing calls
	# render.sh today, so that is a trap for a future caller rather than a path.
	[[ -n "${GAG_PREVIEW_LOCK_HELD:-}" ]] && return 0
	local lock why=""
	lock="$(lock_path)"
	if [[ -z "$lock" ]]; then
		why="no cache directory on this platform"
	elif ! command -v perl >/dev/null 2>&1; then
		why="perl not found"
	fi
	if [[ -n "$why" ]]; then
		# Degrading is right, since a preview tool should not refuse to run, but
		# it is the state this whole section exists to prevent, so say so.
		printf '\033[1;33mwarning:\033[0m %s, so this run holds no lock on %s: a concurrent run will silently render its dashboards instead\n' \
			"$why" "$CLUSTER" >&2
		return 0
	fi
	export GAG_PREVIEW_LOCK_HELD=1
	# perl takes the lock, runs the script as a child, and exits with its status;
	# the lock fd lives in perl and releases when perl exits.
	exec perl -MFcntl=:flock -e '
		my ($path, $cluster) = splice(@ARGV, 0, 2);
		# Same posture as the bash-side degrade above, and the same reason to
		# be loud about it: running on is right, running on in silence is not.
		open(my $fh, ">", $path) or do {
			printf STDERR "\033[1;33mwarning:\033[0m cannot open %s (%s), so this run holds no lock on %s: a concurrent run will silently render its dashboards instead\n", $path, $!, $cluster;
			exec @ARGV;
		};
		my ($start, $next) = (time, 0);
		until (flock($fh, LOCK_EX|LOCK_NB)) {
			my $queued = time - $start;
			if ($queued >= $next) {
				printf STDERR "==> waiting for the %s preview cluster (another session is rendering, queued %ds)...\n", $cluster, $queued;
				$next = $queued + 30;
			}
			select(undef, undef, undef, 1);
		}
		my $queued = time - $start;
		printf STDERR "==> preview cluster acquired after %ds queued\n", $queued if $queued >= 5;
		my $rc = system @ARGV;
		exit 255 if $rc == -1;
		exit($rc & 127 ? 128 + ($rc & 127) : $rc >> 8);
	' "$lock" "$CLUSTER" bash "$0" "$@"
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

	# The real dashboard artifacts, imported via the Grafana sidecar — one
	# labelled ConfigMap per ../grafana-dashboard-*.json.
	local dash base
	for dash in "$MON_DIR"/grafana-dashboard-*.json; do
		base="$(basename "$dash" .json)"
		kubectl create configmap "ag-$base" -n "$MON_NS" \
			--from-file="$base.json=$dash" \
			--dry-run=client -o yaml |
			kubectl label --local -f - grafana_dashboard=1 -o yaml |
			kubectl apply -f -
	done

	kubectl -n team-a rollout status deploy/ag-metrics-exporter --timeout=120s
}

# trim_bottom removes the dead space Grafana leaves between the last panel row
# and the bottom-pinned footer when the render viewport is taller than the
# dashboard. It collapses the image to a 1px column of row averages, finds the
# last row brighter than the dark-theme background, and crops to it (keeping
# full width). No-op if ImageMagick is not installed.
trim_bottom() {
	local png="$1" last
	command -v magick >/dev/null 2>&1 || return 0
	last="$(magick "$png" -colorspace Gray -resize "1x!" -depth 8 txt:- |
		awk 'NR>1 { split($1,a,","); g=$2; gsub(/[()]/,"",g); if (g+0 > 24) last=a[2]+0 } END{print last}')"
	[[ -n "$last" && "$last" -gt 0 ]] || return 0
	magick "$png" -crop "${WIDTH}x$((last + 16))+0+0" +repage "$png"
}

render() {
	log "letting metrics accumulate (${WAIT}s) so rate()/histograms have data"
	sleep "$WAIT"
	mkdir -p "$OUT_DIR"
	port_forward "$RELEASE-grafana" "3000:80"
	local uid out code
	for uid in "${DASH_UIDS[@]}"; do
		out="$OUT_DIR/$uid.png"
		log "rendering dashboard '$uid' to $out"
		code="$(curl -s -u admin:admin -o "$out" -w '%{http_code}' \
			"http://localhost:3000/render/d/$uid/$uid?orgId=1&from=$FROM&to=$TO&width=$WIDTH&height=$HEIGHT&theme=dark&kiosk=1")"
		[[ "$code" == "200" ]] || die "Grafana render of '$uid' returned HTTP $code"
		trim_bottom "$out"
		log "wrote $out ($(wc -c <"$out" | tr -d ' ') bytes)"
	done
	cleanup
}

down() {
	require_cmds
	log "deleting kind cluster '$CLUSTER'"
	kind delete cluster --name "$CLUSTER"
}

main() {
	local action="${1:-up}"
	# Validate before locking, so a typo reports now rather than after queueing
	# behind a render.
	case "$action" in
	up | shot | down) ;;
	*) die "unknown action '$action' (expected: up | shot | down)" ;;
	esac
	# Held for the whole action: the apply and the render have to be one
	# critical section, and `down` must not delete a cluster mid-render.
	serialize_on_cluster "$@"
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
	esac
}

main "$@"
