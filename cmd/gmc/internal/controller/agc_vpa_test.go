package controller

import (
	"context"
	"strings"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// vpaAwareRESTMapper is a RESTMapper that knows the VerticalPodAutoscaler kind — the
// stand-in for a cluster with the vertical-pod-autoscaler CRDs installed. The fake
// client's default mapper knows nothing, which is exactly the CRD-absent cluster.
func vpaAwareRESTMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{verticalPodAutoscalerGVK.GroupVersion()})
	m.Add(verticalPodAutoscalerGVK, apimeta.RESTScopeNamespace)
	return m
}

// autoscalingGateway returns a v2 gateway that opted into managed AGC right-sizing.
func autoscalingGateway(mode gmcv2alpha1.VPAUpdateMode) *gmcv2alpha1.ActionsGateway {
	ag := v2Gateway("gw", "team-a", "github-app", "")
	ag.Spec.AGCAutoscaling = &gmcv2alpha1.AGCVerticalAutoscaling{Mode: mode}
	return ag
}

// TestAGCVPABounds_NoOverrideFloorsUnsetCeilingIsPlatformDefault asserts the
// precedence contract for the common case: a gateway that sets no agcResources gets
// NO minAllowed — the autoscaler is free to shrink the AGC, which is where
// right-sizing has the most to win — while maxAllowed is pinned to the platform
// default limits, because a request may never exceed its own limit.
func TestAGCVPABounds_NoOverrideFloorsUnsetCeilingIsPlatformDefault(t *testing.T) {
	minAllowed, maxAllowed := agcVPABounds(nil)

	assert.Nil(t, minAllowed, "an unset agcResources must impose no autoscaler floor")
	require.NotNil(t, maxAllowed)
	assert.Equal(t, "2", maxAllowed["cpu"], "maxAllowed must be the stamped cpu limit")
	assert.Equal(t, "4Gi", maxAllowed["memory"], "maxAllowed must be the stamped memory limit")
}

// TestAGCVPABounds_ExplicitRequestsBecomeTheFloor asserts that a request the tenant
// explicitly set is honored as the autoscaler's floor rather than silently
// overwritten — the resolution of the agcResources-vs-VPA precedence question.
func TestAGCVPABounds_ExplicitRequestsBecomeTheFloor(t *testing.T) {
	minAllowed, maxAllowed := agcVPABounds(&corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
	})

	require.NotNil(t, minAllowed)
	assert.Equal(t, "3Gi", minAllowed["memory"], "an explicit memory request must become minAllowed")
	assert.NotContains(t, minAllowed, "cpu", "an unset cpu request must impose no cpu floor")
	// The limits were not overridden, so the ceiling stays the platform default.
	assert.Equal(t, "4Gi", maxAllowed["memory"])
}

// TestAGCVPABounds_CeilingTracksEffectiveLimits asserts maxAllowed follows the
// EFFECTIVE limits (default overlaid with the override), not the raw override: the
// effective limits are what the GMC stamps, and the autoscaler must never recommend a
// request above them.
func TestAGCVPABounds_CeilingTracksEffectiveLimits(t *testing.T) {
	_, maxAllowed := agcVPABounds(&corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
	})

	require.NotNil(t, maxAllowed)
	assert.Equal(t, "8Gi", maxAllowed["memory"], "an overridden memory limit must raise the ceiling")
	assert.Equal(t, "2", maxAllowed["cpu"], "an un-overridden cpu limit keeps the platform default ceiling")
}

// TestBuildAGCVerticalPodAutoscaler_TargetsTheAGCDeploymentRequestsOnly locks in the
// shape that makes agcResources and the autoscaler compose instead of compete: the
// object targets the AGC Deployment, names the AGC container exactly as the Deployment
// builder does, and is pinned to RequestsOnly so the stamped limits are never moved.
func TestBuildAGCVerticalPodAutoscaler_TargetsTheAGCDeploymentRequestsOnly(t *testing.T) {
	ag := autoscalingGateway(gmcv2alpha1.VPAUpdateModeRecreate)

	vpa := buildAGCVerticalPodAutoscaler(ag)

	assert.Equal(t, verticalPodAutoscalerGVK, vpa.GroupVersionKind())
	assert.Equal(t, agcNameV2(ag), vpa.GetName())
	assert.Equal(t, ag.Namespace, vpa.GetNamespace())

	targetName, _, err := unstructured.NestedString(vpa.Object, "spec", "targetRef", "name")
	require.NoError(t, err)
	assert.Equal(t, agcNameV2(ag), targetName, "the autoscaler must target the AGC Deployment")

	mode, _, err := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	require.NoError(t, err)
	assert.Equal(t, "Recreate", mode)

	policies, _, err := unstructured.NestedSlice(vpa.Object, "spec", "resourcePolicy", "containerPolicies")
	require.NoError(t, err)
	require.Len(t, policies, 1)
	policy, ok := policies[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "RequestsOnly", policy["controlledValues"],
		"the autoscaler must move requests only, so the agcResources limits stay the hard ceiling")

	// The container name must match the Deployment's container exactly, or the policy
	// silently applies to nothing.
	dep := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2alpha1.SecurityProfileRestricted, nil)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, dep.Spec.Template.Spec.Containers[0].Name, policy["containerName"])
}

// TestBuildAGCVerticalPodAutoscaler_DefaultsToRecommendOnly asserts the safe default:
// a bare agcAutoscaling block (no mode) produces updateMode Off, so opting in observes
// the AGC without ever restarting it.
func TestBuildAGCVerticalPodAutoscaler_DefaultsToRecommendOnly(t *testing.T) {
	ag := autoscalingGateway("")

	mode, _, err := unstructured.NestedString(buildAGCVerticalPodAutoscaler(ag).Object, "spec", "updatePolicy", "updateMode")
	require.NoError(t, err)
	assert.Equal(t, "Off", mode)
}

// TestApplyAGCAutoscaler_CRDAbsentDegradesGracefully is the graceful-degradation test
// for the reconcile helper: on a cluster with no autoscaling.k8s.io CRD, an opt-in
// reports unavailable and returns NO error, so the caller never degrades the gateway.
func TestApplyAGCAutoscaler_CRDAbsentDegradesGracefully(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := autoscalingGateway(gmcv2alpha1.VPAUpdateModeRecreate)
	// No WithRESTMapper: the fake client's default mapper matches no kind at all,
	// which is precisely a cluster without the vertical-pod-autoscaler installed.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	state, err := r.applyOrPruneAGCAutoscaler(context.Background(), ag)
	require.NoError(t, err, "a missing optional CRD must not be a provisioning error")
	assert.True(t, state.requested)
	assert.True(t, state.unavailable)
}

// TestApplyAGCAutoscaler_NotRequestedIsANoOpWithoutTheCRD asserts the prune path is
// also inert on a CRD-less cluster: a gateway that never opted in must not attempt a
// delete against a kind the apiserver does not serve.
func TestApplyAGCAutoscaler_NotRequestedIsANoOpWithoutTheCRD(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("gw", "team-a", "github-app", "")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	state, err := r.applyOrPruneAGCAutoscaler(context.Background(), ag)
	require.NoError(t, err)
	assert.False(t, state.requested)
	assert.False(t, state.unavailable)
}

// TestActionsGatewayV2Reconcile_VPACRDAbsentStaysReady is the end-to-end half of the
// degradation contract (Q360): a gateway that opts into agcAutoscaling on a cluster
// with no VerticalPodAutoscaler CRD is still fully provisioned and Ready, carries the
// advisory AGCAutoscalingUnavailable=True condition with an actionable message, emits
// one Warning Event, and requeues on the slow re-probe cadence — it neither wedges nor
// hot-loops.
func TestActionsGatewayV2Reconcile_VPACRDAbsentStaysReady(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "team-a",
		Labels: map[string]string{gmcv2alpha1.SecurityProfileLabel: gmcv2alpha1.SecurityProfileRestricted},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := autoscalingGateway(gmcv2alpha1.VPAUpdateModeRecreate)
	// A ready AGC Deployment so Ready is decided by provisioning, not by rollout timing.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ag.Namespace},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag, dep).
		WithStatusSubresource(ag).
		Build()
	rec := events.NewFakeRecorder(16)
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", Recorder: rec}

	res := reconcileV2Gateway(t, r, ag.Namespace, ag.Name)
	assert.Equal(t, agcAutoscalerReprobeInterval, res.RequeueAfter,
		"an unsatisfiable opt-in must re-probe on the slow bounded cadence, not hot-loop")

	var fetched gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched))

	ready := apimeta.FindStatusCondition(fetched.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status,
		"a missing optional autoscaler CRD must not gate Ready — the AGC runs on its agcResources sizing")

	degraded := apimeta.FindStatusCondition(fetched.Status.Conditions, gmcv2alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionFalse, degraded.Status, "the gateway must not be Degraded")

	unavailable := apimeta.FindStatusCondition(fetched.Status.Conditions, gmcv2alpha1.ConditionAGCAutoscalingUnavailable)
	require.NotNil(t, unavailable, "the unsatisfiable opt-in must be surfaced, not silent")
	assert.Equal(t, metav1.ConditionTrue, unavailable.Status)
	assert.Equal(t, gmcv2alpha1.ReasonVPACRDNotInstalled, unavailable.Reason)
	assert.Contains(t, unavailable.Message, "vertical-pod-autoscaler", "the message must name the remediation")

	assert.True(t, hasEventContaining(rec, gmcv2alpha1.ReasonVPACRDNotInstalled),
		"expected one Warning event naming the missing CRD")
}

// TestActionsGatewayV2Reconcile_NoOptInReportsAutoscalingDisabled asserts the default
// posture is visible rather than inferred: a gateway that never opted in carries
// AGCAutoscalingUnavailable=False/AGCAutoscalingDisabled and requeues normally.
func TestActionsGatewayV2Reconcile_NoOptInReportsAutoscalingDisabled(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "")
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ag.Namespace},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag, dep).
		WithStatusSubresource(ag).
		Build()
	rec := events.NewFakeRecorder(16)
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", Recorder: rec}

	res := reconcileV2Gateway(t, r, ag.Namespace, ag.Name)
	assert.Zero(t, res.RequeueAfter, "a gateway that did not opt in must not carry the re-probe requeue")

	var fetched gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched))
	cond := apimeta.FindStatusCondition(fetched.Status.Conditions, gmcv2alpha1.ConditionAGCAutoscalingUnavailable)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, gmcv2alpha1.ReasonAGCAutoscalingDisabled, cond.Reason)

	// The condition appearing for the first time in its default state is not a
	// transition worth an Event — otherwise every gateway ever created would carry one
	// line of pure noise in `kubectl describe`.
	assert.False(t, hasEventContaining(rec, gmcv2alpha1.ReasonAGCAutoscalingDisabled),
		"the not-opted-in default must not emit an Event on first observation")
}

// TestApplyAGCAutoscaler_StampsAndPrunes covers the CRD-present lifecycle against a
// RESTMapper that knows the kind: the opt-in stamps an owner-referenced autoscaler,
// and removing the block deletes it again so no stale autoscaler keeps moving the
// AGC's requests.
func TestApplyAGCAutoscaler_StampsAndPrunes(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	scheme.AddKnownTypeWithName(verticalPodAutoscalerGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(verticalPodAutoscalerGVK.GroupVersion().WithKind(verticalPodAutoscalerGVK.Kind+"List"), &unstructured.UnstructuredList{})
	ag := autoscalingGateway(gmcv2alpha1.VPAUpdateModeInitial)
	ag.UID = "gw-uid-1"
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(vpaAwareRESTMapper()).WithObjects(ag).Build()
	rec := events.NewFakeRecorder(8)
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, Recorder: rec}

	state, err := r.applyOrPruneAGCAutoscaler(context.Background(), ag)
	require.NoError(t, err)
	assert.True(t, state.requested)
	assert.False(t, state.unavailable)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(verticalPodAutoscalerGVK)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcVPAName(ag)}, got))
	owners := got.GetOwnerReferences()
	require.Len(t, owners, 1, "the autoscaler must be owner-referenced for cascade GC")
	assert.Equal(t, ag.Name, owners[0].Name)
	assert.True(t, hasEventContaining(rec, gmcv2alpha1.ReasonAGCAutoscalingActive),
		"the precedence resolution must be announced when the autoscaler first appears")

	// Removing the opt-in prunes it.
	ag.Spec.AGCAutoscaling = nil
	state, err = r.applyOrPruneAGCAutoscaler(context.Background(), ag)
	require.NoError(t, err)
	assert.False(t, state.requested)
	err = c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcVPAName(ag)}, got)
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil,
		"removing spec.agcAutoscaling must delete the managed autoscaler")
}

// hasEventContaining drains a fake recorder looking for an event whose text contains
// substr.
func hasEventContaining(rec *events.FakeRecorder, substr string) bool {
	for {
		select {
		case msg := <-rec.Events:
			if strings.Contains(msg, substr) {
				return true
			}
		default:
			return false
		}
	}
}
