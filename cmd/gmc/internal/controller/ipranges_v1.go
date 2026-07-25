package controller

import (
	"context"
	"log/slog"
	"net"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The v1 ActionsGateway half of the shared GitHub IP-range refresh loop. Kept in
// its own file so the v1 sunset (Q273) deletes it whole, leaving ipranges.go — the
// fetcher, the cache, the retry loop and the v2 passes — free of v1alpha1.

// refreshV1ProxyNetworkPolicies patches every managed v1 ActionsGateway's proxy
// NetworkPolicy with the current CIDR set. A List failure is returned (it leaves no
// policy patched, which reconcileInitial retries); per-CR patch failures are logged
// and skipped so one bad CR cannot block the rest.
func (r *IPRangeReconciler) refreshV1ProxyNetworkPolicies(ctx context.Context, log *slog.Logger, cidrs []net.IPNet) error {
	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList); err != nil {
		log.Error("failed to list ActionsGateways", "error", err)
		return err
	}

	for i := range agList.Items {
		ag := &agList.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue // skip CRs being deleted; their NetworkPolicy is already being removed
		}
		if ag.Spec.Proxy.ManagedNetworkPolicy != nil && !*ag.Spec.Proxy.ManagedNetworkPolicy {
			continue
		}
		if err := r.patchNetworkPolicy(ctx, ag, cidrs); err != nil {
			log.Error("failed to patch NetworkPolicy", "namespace", ag.Namespace, "name", ag.Name, "error", err)
		}
	}
	return nil
}

func (r *IPRangeReconciler) patchNetworkPolicy(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, cidrs []net.IPNet) error {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: npProxyName}, &np); err != nil {
		return client.IgnoreNotFound(err) // NetworkPolicy may not exist yet or is being removed
	}

	desired := buildProxyNetworkPolicy(ag, cidrs)
	np.Spec.Egress = desired.Spec.Egress
	np.Spec.Ingress = desired.Spec.Ingress

	if err := r.Update(ctx, &np); err != nil {
		return err
	}
	if r.Metrics != nil {
		r.Metrics.IPRangeUpdates.WithLabelValues(ag.Namespace).Inc()
	}
	return nil
}
