//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Managed AGC right-sizing (Q360). The suite installs a stub autoscaling.k8s.io
// VerticalPodAutoscaler CRD (testdata/autoscaling-crds), so these tests cover the
// CRD-PRESENT half end-to-end: the opt-in stamps an owner-referenced autoscaler with
// the bounds derived from spec.agcResources, and removing the opt-in prunes it. The
// CRD-ABSENT degradation is unit-tested in the controller package
// (TestActionsGatewayV2Reconcile_VPACRDAbsentStaysReady), where the RESTMapper can be
// made to match no kind at all.

var vpaGVK = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"}

func getVPA(t *testing.T, ns, name string) (*unstructured.Unstructured, error) {
	t.Helper()
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(vpaGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vpa)
	return vpa, err
}

// TestV2_ActionsGateway_AGCAutoscalingStampsVPA proves the per-gateway opt-in: a
// gateway with spec.agcAutoscaling gets a VerticalPodAutoscaler next to its AGC
// Deployment, owner-referenced for cascade GC, targeting that Deployment, restricted
// to the AGC container's requests, and bounded by the agcResources precedence rules —
// an explicitly set request becomes minAllowed, and the effective limits become
// maxAllowed.
func TestV2_ActionsGateway_AGCAutoscalingStampsVPA(t *testing.T) {
	const ns = "v2-ag-vpa"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	ag.Spec.AGCAutoscaling = &v2alpha1.AGCVerticalAutoscaling{Mode: v2alpha1.VPAUpdateModeRecreate}
	// An explicit memory request (the floor) plus an explicit memory limit (the ceiling).
	ag.Spec.AGCResources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	var vpa *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var err error
		vpa, err = getVPA(t, ns, "gw-agc")
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "the opt-in should stamp a VerticalPodAutoscaler next to the AGC Deployment")

	owners := vpa.GetOwnerReferences()
	require.Len(t, owners, 1, "the autoscaler must be owner-referenced for cascade GC")
	assert.Equal(t, "gw", owners[0].Name)
	assert.Equal(t, "ActionsGateway", owners[0].Kind)

	target, _, err := unstructured.NestedString(vpa.Object, "spec", "targetRef", "name")
	require.NoError(t, err)
	assert.Equal(t, "gw-agc", target)

	mode, _, err := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	require.NoError(t, err)
	assert.Equal(t, "Recreate", mode)

	policies, _, err := unstructured.NestedSlice(vpa.Object, "spec", "resourcePolicy", "containerPolicies")
	require.NoError(t, err)
	require.Len(t, policies, 1)
	policy, ok := policies[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "agc", policy["containerName"])
	assert.Equal(t, "RequestsOnly", policy["controlledValues"],
		"the autoscaler must move requests only so the agcResources limits stay the ceiling")

	minAllowed, ok := policy["minAllowed"].(map[string]interface{})
	require.True(t, ok, "an explicit agcResources request must become the autoscaler floor")
	assert.Equal(t, "1Gi", minAllowed["memory"])
	maxAllowed, ok := policy["maxAllowed"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "6Gi", maxAllowed["memory"], "the effective memory limit must be the ceiling")
	assert.Equal(t, "2", maxAllowed["cpu"], "the un-overridden cpu limit keeps the platform default ceiling")

	// The gateway reports the opt-in as satisfied, not as an unavailable trade-off.
	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionAGCAutoscalingUnavailable)
		return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == v2alpha1.ReasonAGCAutoscalingActive
	}, 15*time.Second, 100*time.Millisecond, "AGCAutoscalingUnavailable should be False/AGCAutoscalingActive")
}

// TestV2_ActionsGateway_AGCAutoscalingRemovalPrunesVPA proves the opt-in is
// reversible: clearing spec.agcAutoscaling deletes the managed autoscaler, so no stale
// autoscaler is left moving the AGC's requests after the tenant opted out.
func TestV2_ActionsGateway_AGCAutoscalingRemovalPrunesVPA(t *testing.T) {
	const ns = "v2-ag-vpa-prune"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	ag.Spec.AGCAutoscaling = &v2alpha1.AGCVerticalAutoscaling{}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	require.Eventually(t, func() bool {
		_, err := getVPA(t, ns, "gw-agc")
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "the opt-in should stamp a VerticalPodAutoscaler")

	// A bare block defaults to recommendation-only, so opting in never restarts the AGC.
	vpa, err := getVPA(t, ns, "gw-agc")
	require.NoError(t, err)
	mode, _, err := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	require.NoError(t, err)
	assert.Equal(t, "Off", mode, "a bare agcAutoscaling block must default to recommendation-only")

	var got v2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got))
	got.Spec.AGCAutoscaling = nil
	require.NoError(t, k8sClient.Update(ctx, &got))

	require.Eventually(t, func() bool {
		_, err := getVPA(t, ns, "gw-agc")
		return apierrors.IsNotFound(err)
	}, 15*time.Second, 100*time.Millisecond, "removing spec.agcAutoscaling should delete the managed autoscaler")

	require.Eventually(t, func() bool {
		var g v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &g); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(g.Status.Conditions, v2alpha1.ConditionAGCAutoscalingUnavailable)
		return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == v2alpha1.ReasonAGCAutoscalingDisabled
	}, 15*time.Second, 100*time.Millisecond, "the condition should return to False/AGCAutoscalingDisabled")
}
