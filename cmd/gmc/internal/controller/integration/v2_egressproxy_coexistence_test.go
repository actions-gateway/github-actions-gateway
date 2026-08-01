//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q582: a v1 inline proxy pool and a v2 EgressProxy pool share a namespace for the
// whole coexistence window of a migration. Both used to stamp
// `app: actions-gateway-proxy`, which is the sole key of v1's PDB selector, v1's
// Deployment selector, and v1's hostname anti-affinity term — so each pool's pods fell
// under the other's PDB, both HPAs wedged on AmbiguousSelector, and the two pools
// repelled each other off every node.
//
// These run against the real apiserver because the two things worth measuring are
// apiserver behaviour: which pods a live selector actually returns, and that
// spec.selector really is immutable (which is what forces the recreate path).

// createProxyPodFromTemplate creates a pod carrying a Deployment's pod-template
// labels — the pods that Deployment's ReplicaSet would create. envtest runs no
// scheduler, so it stays Pending; only its labels matter here.
func createProxyPodFromTemplate(t *testing.T, ns, name string, dep *appsv1.Deployment) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: dep.Spec.Template.Labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "proxy:test"}}},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
}

// podsCoveredBy lists the names of the pods in ns that sel selects, the way the
// disruption controller and the HPA controller resolve their own selectors.
func podsCoveredBy(t *testing.T, ns string, sel *metav1.LabelSelector) []string {
	t.Helper()
	ls, err := metav1.LabelSelectorAsSelector(sel)
	require.NoError(t, err)
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: ls}))
	names := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	return names
}

func TestV2_Coexistence_EachPoolsSelectorsCoverOnlyItsOwnPods(t *testing.T) {
	const ns = "v2-coexist-selectors"
	const epName = "legacy-egress"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	// The migration shape: the v1 gateway keeps running (it is what rollback returns
	// to) and the extracted EgressProxy is applied beside it.
	ag := newActionsGateway("legacy", ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	ep := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: epName, Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	startGMCReconciler(t, nil)
	startEgressProxyReconciler(t, nil)

	v1Dep := reconciledDeployment(t, ns, proxyName)
	v2Dep := reconciledDeployment(t, ns, proxyChildName(epName))

	createProxyPodFromTemplate(t, ns, "v1-proxy-pod", v1Dep)
	createProxyPodFromTemplate(t, ns, "v2-proxy-pod", v2Dep)

	// PodDisruptionBudgets. Two PDBs covering one pod is not a mis-budgeted drain: the
	// eviction API refuses to evict such a pod at all, so a node drain never completes.
	v1PDB := reconciledPDB(t, ns, proxyName)
	v2PDB := reconciledPDB(t, ns, proxyChildName(epName))
	assert.Equal(t, []string{"v1-proxy-pod"}, podsCoveredBy(t, ns, v1PDB.Spec.Selector),
		"the v1 PDB must cover the v1 pool's pods and nothing else")
	assert.Equal(t, []string{"v2-proxy-pod"}, podsCoveredBy(t, ns, v2PDB.Spec.Selector),
		"the v2 PDB must cover the v2 pool's pods and nothing else")

	// Deployment selectors — what each pool's HPA resolves through the scale
	// subresource. The HPA controller lists the pods its scale target selects and sets
	// ScalingActive=False/AmbiguousSelector when any of them is controlled by a second
	// HPA, which wedges BOTH pools rather than one. Disjoint pod sets here is that
	// condition not being met.
	assert.Equal(t, []string{"v1-proxy-pod"}, podsCoveredBy(t, ns, v1Dep.Spec.Selector))
	assert.Equal(t, []string{"v2-proxy-pod"}, podsCoveredBy(t, ns, v2Dep.Spec.Selector))
}

// TestV2_EgressProxy_RecreatesAPoolWhoseSelectorPredatesTheNarrowing covers the
// upgrade path for an install that already runs a v2 pool: its Deployment carries the
// old two-label selector, spec.selector is immutable, so the reconciler must delete and
// recreate it rather than wedge Degraded on a rejected patch.
func TestV2_EgressProxy_RecreatesAPoolWhoseSelectorPredatesTheNarrowing(t *testing.T) {
	const ns = "v2-coexist-selector-migration"
	const epName = "pre-existing"
	createNamespace(t, ns)

	name := proxyChildName(epName)
	legacySelector := map[string]string{
		"app":                "actions-gateway-proxy",
		proxyIdentityLabel(): epName,
	}
	legacy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: legacySelector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: legacySelector},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "proxy:test"}}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, legacy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), legacy) })
	legacyUID := legacy.UID

	// Measure the constraint rather than assuming it: patching the selector is what a
	// naive fix would do, and the apiserver rejects it. If this ever stops erroring,
	// the recreate path above it is dead weight and should go.
	narrowed := legacy.DeepCopy()
	narrowed.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{proxyIdentityLabel(): epName}}
	narrowed.Spec.Template.Labels = map[string]string{proxyIdentityLabel(): epName}
	require.Error(t, k8sClient.Update(ctx, narrowed),
		"Deployment.spec.selector must be immutable — the premise of the recreate path")

	ep := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: epName, Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	startEgressProxyReconciler(t, nil)

	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep); err != nil {
			return false
		}
		return dep.UID != legacyUID
	}, 30*time.Second, 200*time.Millisecond, "the pre-existing pool must be recreated, not patched")

	assert.Equal(t, map[string]string{proxyIdentityLabel(): epName}, dep.Spec.Selector.MatchLabels,
		"the replacement carries the narrowed selector")
	assert.NotContains(t, dep.Spec.Template.Labels, "app",
		"the replacement's pods must not wear v1's pool label")
	assert.True(t, hasControllerOwnerRef(dep.OwnerReferences, epName),
		"the replacement is owned by the EgressProxy, so cascade GC still reclaims it")
}

// reconciledDeployment / reconciledPDB return the named object once the reconciler
// has created it.
func reconciledDeployment(t *testing.T, ns, name string) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep) == nil
	}, 30*time.Second, 200*time.Millisecond, "Deployment %s/%s should be created", ns, name)
	return &dep
}

func reconciledPDB(t *testing.T, ns, name string) *policyv1.PodDisruptionBudget {
	t.Helper()
	var pdb policyv1.PodDisruptionBudget
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pdb) == nil
	}, 30*time.Second, 200*time.Millisecond, "PDB %s/%s should be created", ns, name)
	return &pdb
}
