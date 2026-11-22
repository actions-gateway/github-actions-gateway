//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestV2_EgressProxy_HPABoundsFollowTheSpec pins applyHPA's update path: changing an
// existing pool's minReplicas and maxReplicas patches the live HPA's bounds.
// TestV2_EgressProxy_ReconcilerPreservesHPAScaleOut reads them only at create. v1's
// clamp twin has no v2 counterpart, because the EgressProxy CRD rejects
// minReplicas > maxReplicas at admission.
func TestV2_EgressProxy_HPABoundsFollowTheSpec(t *testing.T) {
	const ns = "v2-ep-hpa-bounds"
	createNamespace(t, ns)
	ep := newV2EgressProxyObject(egressProxyName, ns)
	ep.Spec.MinReplicas, ep.Spec.MaxReplicas = ptr32(2), ptr32(6)
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	startEgressProxyReconciler(t, nil)

	key := types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}
	bounds := func(minR, maxR int32) func() bool {
		return func() bool {
			var hpa autoscalingv2.HorizontalPodAutoscaler
			return k8sClient.Get(ctx, key, &hpa) == nil &&
				hpa.Spec.MinReplicas != nil && *hpa.Spec.MinReplicas == minR && hpa.Spec.MaxReplicas == maxR
		}
	}
	require.Eventually(t, bounds(2, 6), 15*time.Second, 100*time.Millisecond, "the HPA should start at 2..6")

	require.Eventually(t, func() bool {
		var live v2.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyName}, &live); err != nil {
			return false
		}
		live.Spec.MinReplicas, live.Spec.MaxReplicas = ptr32(3), ptr32(8)
		return k8sClient.Update(ctx, &live) == nil
	}, 5*time.Second, 50*time.Millisecond, "update the EgressProxy bounds")
	require.Eventually(t, bounds(3, 8), 15*time.Second, 100*time.Millisecond, "the HPA should follow the spec to 3..8")
}

// TestV2_EgressProxy_PDBDriftIsPatchedBack pins applyPDB's update path: the
// reconciler owns the pool's PDB spec, so an out-of-band edit is reverted.
func TestV2_EgressProxy_PDBDriftIsPatchedBack(t *testing.T) {
	const ns = "v2-ep-pdb-drift"
	createNamespace(t, ns)
	ep := newV2EgressProxyObject(egressProxyName, ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	startEgressProxyReconciler(t, nil)

	key := types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}
	minAvailable := func(want int32) func() bool {
		return func() bool {
			var pdb policyv1.PodDisruptionBudget
			return k8sClient.Get(ctx, key, &pdb) == nil && pdb.Spec.MinAvailable != nil && pdb.Spec.MinAvailable.IntVal == want
		}
	}
	require.Eventually(t, minAvailable(1), 15*time.Second, 100*time.Millisecond, "the PDB should keep one replica available")

	require.Eventually(t, func() bool {
		var pdb policyv1.PodDisruptionBudget
		if err := k8sClient.Get(ctx, key, &pdb); err != nil {
			return false
		}
		zero := intstr.FromInt32(0)
		pdb.Spec.MinAvailable = &zero
		return k8sClient.Update(ctx, &pdb) == nil
	}, 5*time.Second, 50*time.Millisecond, "drift the PDB out of band")
	require.Eventually(t, minAvailable(1), 15*time.Second, 100*time.Millisecond, "the reconciler should patch the PDB back")
}
