package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// injectRestartedAt stamps the pod-template annotation `kubectl rollout restart`
// writes, exactly as kubectl does: a patch of
// .spec.template.metadata.annotations on the live Deployment.
func injectRestartedAt(t *testing.T, c client.Client, key types.NamespacedName, at string) {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[kubectlRestartedAtAnnotation] = at
	require.NoError(t, c.Update(context.Background(), &dep))
}

// TestApplyDeployment_PreservesRestartedAt is the Q552 regression: an operator
// runs `kubectl rollout restart` on the v1 AGC Deployment and the next GMC
// reconcile must not revert the restartedAt annotation — reverting it rolls the
// pod template back to the pre-restart hash, so the Deployment controller never
// creates a new ReplicaSet and the restart is a silent no-op.
func TestApplyDeployment_PreservesRestartedAt(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()
	key := types.NamespacedName{Namespace: ag.Namespace, Name: agcAppName}

	desired := func() *appsv1.Deployment {
		return buildAGCDeployment(ag, "agc:test", "proxy:8080", nil)
	}
	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))
	injectRestartedAt(t, c, key, "2026-07-31T12:00:00Z")

	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	assert.Equal(t, "2026-07-31T12:00:00Z", dep.Spec.Template.Annotations[kubectlRestartedAtAnnotation],
		"an operator-injected restartedAt must survive reconcile, or the rollout restart is a no-op")
	// The builder still owns the rest of the template.
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "agc:test", dep.Spec.Template.Spec.Containers[0].Image)
}

// TestV2ApplyDeployment_PreservesRestartedAt is the v2 AGC half of Q552 — the
// tier the dogfood `agc-bounce` escape hatch actually targets.
func TestV2ApplyDeployment_PreservesRestartedAt(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ag := v2Gateway("tenant", "tenant-ns", "gh-app-creds", "")
	ag.UID = "ag-uid-1"
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}

	desired := func() *appsv1.Deployment {
		return buildAGCDeploymentV2(ag, "agc:test", nil, "restricted", nil)
	}
	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))
	injectRestartedAt(t, c, key, "2026-07-31T12:00:00Z")

	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	assert.Equal(t, "2026-07-31T12:00:00Z", dep.Spec.Template.Annotations[kubectlRestartedAtAnnotation])
}

// TestEgressProxyApplyDeployment_PreservesRestartedAt covers the HPA-targeted
// proxy pool, which reaches the template through assignHPATargetDeploymentSpec
// rather than a whole-spec replace.
func TestEgressProxyApplyDeployment_PreservesRestartedAt(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", nil)
	ep.UID = "ep-uid-1"
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	key := types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}

	desired := func() *appsv1.Deployment {
		return buildEgressProxyDeployment(ep, "proxy:test", []string{"github.com"})
	}
	require.NoError(t, r.applyDeployment(context.Background(), ep, desired()))
	injectRestartedAt(t, c, key, "2026-07-31T12:00:00Z")

	require.NoError(t, r.applyDeployment(context.Background(), ep, desired()))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	assert.Equal(t, "2026-07-31T12:00:00Z", dep.Spec.Template.Annotations[kubectlRestartedAtAnnotation])
}

// TestApplyDeployment_RevertsUnrelatedTemplateDrift pins the other half of the
// contract: tolerating restartedAt must not turn the pod template into a
// free-for-all. Any other hand-edited template annotation is still reverted.
func TestApplyDeployment_RevertsUnrelatedTemplateDrift(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()
	key := types.NamespacedName{Namespace: ag.Namespace, Name: agcAppName}

	desired := func() *appsv1.Deployment {
		return buildAGCDeployment(ag, "agc:test", "proxy:8080", nil)
	}
	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["sidecar.istio.io/inject"] = "false"
	dep.Spec.Template.Spec.Containers[0].Image = "attacker/agc:evil"
	require.NoError(t, c.Update(context.Background(), &dep))

	require.NoError(t, r.applyDeployment(context.Background(), ag, desired()))

	require.NoError(t, c.Get(context.Background(), key, &dep))
	assert.NotContains(t, dep.Spec.Template.Annotations, "sidecar.istio.io/inject",
		"an unlisted hand-edited template annotation must still be reverted")
	assert.Equal(t, "agc:test", dep.Spec.Template.Spec.Containers[0].Image)
}

// TestAssignManagedPodTemplate_DoesNotMutateDesired guards the aliasing hazard:
// the preserved annotation must land on a copy, never in the builder's own map.
func TestAssignManagedPodTemplate_DoesNotMutateDesired(t *testing.T) {
	live := corev1.PodTemplateSpec{}
	live.Annotations = map[string]string{kubectlRestartedAtAnnotation: "2026-07-31T12:00:00Z"}
	desired := corev1.PodTemplateSpec{}
	desired.Annotations = map[string]string{"actions-gateway/github-app-secret": "creds"}

	assignManagedPodTemplate(&live, desired)

	assert.Equal(t, "2026-07-31T12:00:00Z", live.Annotations[kubectlRestartedAtAnnotation])
	assert.Equal(t, "creds", live.Annotations["actions-gateway/github-app-secret"])
	assert.NotContains(t, desired.Annotations, kubectlRestartedAtAnnotation,
		"the builder's annotation map must not be written through")
}

// TestAssignManagedPodTemplate_NilAnnotations covers the create path, where the
// live template carries no annotations at all.
func TestAssignManagedPodTemplate_NilAnnotations(t *testing.T) {
	var live corev1.PodTemplateSpec
	desired := corev1.PodTemplateSpec{}
	desired.Labels = map[string]string{"app": "agc"}

	assignManagedPodTemplate(&live, desired)

	assert.Empty(t, live.Annotations)
	assert.Equal(t, map[string]string{"app": "agc"}, live.Labels)
}
