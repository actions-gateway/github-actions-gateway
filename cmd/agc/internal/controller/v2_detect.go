package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// RunnerSetInstalled reports whether the actions-gateway.com/v2alpha1 RunnerSet
// CRD is installed and served by the apiserver. The AGC calls it once at startup
// to decide whether to register the v2 RunnerSetReconciler: the RunnerSet CRD
// (and the sibling ActionsGateway/EgressProxy/RunnerTemplate kinds the reconciler
// watches) ships in the opt-in actions-gateway-crds-v2 Helm chart, separate from
// the main chart. On a v1-only install (the main actions-gateway chart alone)
// those kinds are absent, so registering the RunnerSetReconciler unconditionally
// makes its informer cache never sync — mgr.Start then exits(1) after the
// cache-sync deadline (~2m), crash-looping the AGC (Q261). Detecting RunnerSet is
// sufficient: it shares the group and chart with the reconciler's other watched
// kinds, so the whole v2 CRD set stands or falls together.
//
// A NoMatch (the kind is absent) returns (false, nil) — the expected, non-error
// v1-only state. Any other discovery error is returned so the caller can fail fast
// rather than silently disabling the v2 reconciler on a transient apiserver
// hiccup. Detection happens at startup, so installing the v2 CRDs later requires
// an AGC restart to enable the RunnerSetReconciler.
func RunnerSetInstalled(mapper meta.RESTMapper) (bool, error) {
	gvk := agcv2alpha1.GroupVersion.WithKind("RunnerSet")
	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking REST mapping for %s: %w", gvk, err)
	}
	return true, nil
}
