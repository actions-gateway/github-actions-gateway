//go:build integration

package integration_test

import (
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q282 pod scheduling pass-through, exercised against the real apiserver. Three things
// only envtest can settle, each of which a fake client or a hand-built Go struct would
// silently get wrong:
//
//  1. Structural-schema PRUNING. The CRD schema is generated from corev1.Affinity /
//     corev1.Toleration. If a field failed to generate, the apiserver prunes it on
//     write and the reader sees the zero value — a green unit test over a green build.
//  2. The `podAntiAffinity: {}` OPT-OUT. The single-node-tenant-pool escape hatch
//     (Q243) depends on an empty object surviving a write/read round-trip as a
//     NON-NIL pointer. Whether the apiserver preserves or elides an empty known
//     object is an apiserver question, not a Go question.

// tenantPoolScheduling is the Q243 egress-IP pinning shape: pin the pool to one node
// pool (nodeSelector), tolerate that pool's dedication taint, and express the same
// constraint as a nodeAffinity so the composition rule is exercised too.
func tenantPoolScheduling() *v2.PodScheduling {
	return &v2.PodScheduling{
		NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "pool-tenant-a"},
		Tolerations: []corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "tenant-a",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "cloud.google.com/gke-nodepool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"pool-tenant-a"},
						}},
					}},
				},
			},
		},
	}
}

// TestV2_EgressProxy_SchedulingPlumbedToDeployment: the fields survive the apiserver
// (no pruning) and reach the proxy Deployment's pod spec, with the built-in cross-node
// anti-affinity preserved alongside the supplied nodeAffinity.
func TestV2_EgressProxy_SchedulingPlumbedToDeployment(t *testing.T) {
	const ns = "v2-ep-sched"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: ns},
		Spec:       v2.EgressProxySpec{Scheduling: tenantPoolScheduling()},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	// The CR itself round-trips unpruned.
	var stored v2.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pinned"}, &stored))
	require.NotNil(t, stored.Spec.Scheduling, "spec.scheduling must survive the apiserver (schema pruning guard)")
	assert.Equal(t, "pool-tenant-a", stored.Spec.Scheduling.NodeSelector["cloud.google.com/gke-nodepool"])
	require.Len(t, stored.Spec.Scheduling.Tolerations, 1)
	assert.Equal(t, "dedicated", stored.Spec.Scheduling.Tolerations[0].Key)
	require.NotNil(t, stored.Spec.Scheduling.Affinity)
	require.NotNil(t, stored.Spec.Scheduling.Affinity.NodeAffinity)

	// …and reaches the Deployment the reconciler builds.
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pinned-proxy"}, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy Deployment should be created")

	pod := dep.Spec.Template.Spec
	assert.Equal(t, "pool-tenant-a", pod.NodeSelector["cloud.google.com/gke-nodepool"])
	require.Len(t, pod.Tolerations, 1)
	assert.Equal(t, "tenant-a", pod.Tolerations[0].Value)
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.NodeAffinity, "supplied nodeAffinity reaches the pod")
	require.NotNil(t, pod.Affinity.PodAntiAffinity, "built-in cross-node spread is preserved")
	assert.Len(t, spreadTerms(pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution), 1)
	assertWorkerSeparation(t, &dep)
}

// TestV2_EgressProxy_EmptyPodAntiAffinityOptsOutOfSpread is the crux of the Q243
// single-node-tenant-pool escape hatch: an explicit `podAntiAffinity: {}` must survive
// the apiserver as a NON-NIL pointer with no terms, so the builder reads it as "the
// author owns anti-affinity" and drops the built-in required spread. If the apiserver
// elided the empty object, the pointer would come back nil, the built-in required term
// would be reinstated, and every replica after the first would wedge in Pending on a
// one-node pool — a silent regression no unit test could catch.
func TestV2_EgressProxy_EmptyPodAntiAffinityOptsOutOfSpread(t *testing.T) {
	const ns = "v2-ep-sched-optout"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "onenode", Namespace: ns},
		Spec: v2.EgressProxySpec{
			Scheduling: &v2.PodScheduling{
				Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	// The empty object survives storage as a non-nil pointer.
	var stored v2.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "onenode"}, &stored))
	require.NotNil(t, stored.Spec.Scheduling)
	require.NotNil(t, stored.Spec.Scheduling.Affinity)
	require.NotNil(t, stored.Spec.Scheduling.Affinity.PodAntiAffinity,
		"an explicit empty podAntiAffinity must survive the apiserver as non-nil — the opt-out depends on it")

	// …and the built-in required spread is therefore gone from the Deployment.
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "onenode-proxy"}, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy Deployment should be created")

	pod := dep.Spec.Template.Spec
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Empty(t, spreadTerms(pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution),
		"explicit empty podAntiAffinity must suppress the built-in required cross-node spread")
	assertWorkerSeparation(t, &dep)
	assert.Empty(t, pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
}

// TestV2_EgressProxy_NoSchedulingLeavesBuiltInSpread pins the backward-compatible
// default against the real apiserver: an EgressProxy with no spec.scheduling still gets
// the required cross-node anti-affinity and no placement fields.
func TestV2_EgressProxy_NoSchedulingLeavesBuiltInSpread(t *testing.T) {
	const ns = "v2-ep-sched-default"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &v2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "plain-proxy"}, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy Deployment should be created")

	pod := dep.Spec.Template.Spec
	assert.Empty(t, pod.NodeSelector)
	assert.Empty(t, pod.Tolerations)
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Len(t, spreadTerms(pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution), 1)
	assertWorkerSeparation(t, &dep)
	assert.Nil(t, pod.Affinity.NodeAffinity)
}
