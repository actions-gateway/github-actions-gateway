//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q582: an EgressProxy pool used to select its pods by `app: actions-gateway-proxy`
// alone, the label 1.x's inline proxy pool also keyed its PDB, Deployment selector,
// and anti-affinity on — so two pools in one namespace covered each other's pods. A
// pool whose selector predates the per-EgressProxy identity label is recreated.
//
// These run against the real apiserver because the two things worth measuring are
// apiserver behaviour: which pods a live selector actually returns, and that
// spec.selector really is immutable (which is what forces the recreate path).

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

	ep := &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: epName, Namespace: ns}}
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
