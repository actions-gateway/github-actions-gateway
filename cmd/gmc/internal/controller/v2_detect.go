package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// v2DetectKinds are the actions-gateway.com/v2alpha1 kinds the GMC's v2
// controllers (ActionsGatewayV2Reconciler, EgressProxyReconciler) and the
// IPRange reconciler's v2 NetworkPolicy refresh paths depend on. They ship in
// the opt-in actions-gateway-crds-v2 Helm chart, separate from the main chart
// (which would otherwise push its Helm release Secret past the 1 MiB limit), so
// a plain `helm install actions-gateway` yields a cluster where these kinds are
// not served. The RunnerSet/RunnerTemplate/ClusterRunnerTemplate kinds share
// the same group and chart, so ActionsGateway + EgressProxy being served is a
// sufficient signal that the whole v2 CRD set is installed.
var v2DetectKinds = []string{"ActionsGateway", "EgressProxy"}

// V2alpha1Installed reports whether the opt-in actions-gateway.com/v2alpha1 CRDs
// are installed and served by the apiserver. The GMC calls it once at startup to
// decide whether to register the v2 controllers and enable the IPRange
// reconciler's v2 refresh paths: a v1-only install (the main chart without
// actions-gateway-crds-v2) must come up clean rather than spinning a
// source.Kind retry loop and logging "no matches for kind" on every reconcile.
//
// A NoMatch (the kinds are absent) returns (false, nil) — the expected,
// non-error v1-only state. Any other discovery error is returned so the caller
// can fail fast rather than silently disabling v2 on a transient apiserver
// hiccup. Detection happens at startup, so installing the v2 CRDs later requires
// a GMC restart to enable the v2 controllers.
func V2alpha1Installed(mapper meta.RESTMapper) (bool, error) {
	for _, kind := range v2DetectKinds {
		gvk := gmcv2alpha1.GroupVersion.WithKind(kind)
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			if meta.IsNoMatchError(err) {
				return false, nil
			}
			return false, fmt.Errorf("checking REST mapping for %s: %w", gvk, err)
		}
	}
	return true, nil
}
