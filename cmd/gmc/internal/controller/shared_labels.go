package controller

import (
	"strings"

	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcnames "github.com/actions-gateway/github-actions-gateway/gmc/names"
)

// Label vocabulary shared by every GMC-managed object. The ActionsGateway and
// EgressProxy reconcilers both stamp these; only the per-object label *assembly*
// (which carries the CR's own name and is therefore typed on one API version) lives
// with its version's builder.
const (
	labelManagedBy    = "app.kubernetes.io/managed-by"
	labelManagerValue = "actions-gateway-gmc"

	// app.kubernetes.io/component values for the objects the GMC creates, and the
	// app.kubernetes.io/name value for worker-tier objects (the AGC/proxy names are
	// agcAppName/proxyAppName below). Stamped via the recommendedLabels helpers.
	componentControllerLabel = "controller"
	componentProxyLabel      = "proxy"
	componentRunnerLabel     = "runner"
	appNameWorker            = "actions-runner"

	// agcAppName / proxyAppName are the app.kubernetes.io/name values. Workload
	// names are derived per CR; these stay the recommended-label app identity.
	proxyAppName = gmcnames.ProxyName
	agcAppName   = agcnames.ControllerName

	// labelComponent / componentWorkload identify AGC and worker pods as "workload" for
	// NetworkPolicy podSelector matching.
	labelComponent    = "actions-gateway/component"
	componentWorkload = "workload"

	// labelRunnerSetOwner names the RunnerSet that owns a v2 worker pod; the AGC
	// stamps it (provisioner.LabelRunnerSet).
	labelRunnerSetOwner = "actions-gateway.com/runner-set"
)

// copyRecommendedLabels copies the app.kubernetes.io/* recommended metadata from
// src (an object's metadata labels) into dst (a pod template's labels) without
// overwriting any functional selector label already in dst. Used to carry a
// Deployment's recommended labels onto its pods so the pods group with their owner
// under Lens/k9s/Argo and Prometheus relabel rules.
func copyRecommendedLabels(dst, src map[string]string) {
	for k, v := range src {
		if strings.HasPrefix(k, "app.kubernetes.io/") {
			if _, ok := dst[k]; !ok {
				dst[k] = v
			}
		}
	}
}
