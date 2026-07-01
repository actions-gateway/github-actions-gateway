package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
)

// TestApplyNetworkPolicy_CreateThenPatch verifies the create path stamps the
// desired labels/spec and a re-apply with a changed spec patches it in place.
func TestApplyNetworkPolicy_CreateThenPatch(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	desired := buildProxyNetworkPolicy(ag, nil)
	require.NoError(t, r.applyNetworkPolicy(context.Background(), desired))

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &np))
	assert.Equal(t, desired.Labels, np.Labels)
	assert.Equal(t, desired.Spec.PolicyTypes, np.Spec.PolicyTypes)

	// Re-apply with an extra label; the patch must land.
	desired2 := buildProxyNetworkPolicy(ag, nil)
	desired2.Labels["extra"] = "v1"
	require.NoError(t, r.applyNetworkPolicy(context.Background(), desired2))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &np))
	assert.Equal(t, "v1", np.Labels["extra"])
}

// TestApplyPDB_CreateThenPatch verifies the PDB is created with the desired
// MinAvailable/selector and a re-apply patches a changed MinAvailable.
func TestApplyPDB_CreateThenPatch(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	desired := buildPDB(ag)
	require.NoError(t, r.applyPDB(context.Background(), desired))

	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &pdb))
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable)
	assert.Equal(t, map[string]string{"app": proxyAppName}, pdb.Spec.Selector.MatchLabels)

	changed := desired.DeepCopy()
	two := intstr.FromInt32(2)
	changed.Spec.MinAvailable = &two
	require.NoError(t, r.applyPDB(context.Background(), changed))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &pdb))
	assert.Equal(t, intstr.FromInt32(2), *pdb.Spec.MinAvailable)
}

// TestApplyHPA_CreateThenPatch verifies the HPA is created with the desired
// min/max replicas and a re-apply patches a changed MaxReplicas.
func TestApplyHPA_CreateThenPatch(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	desired := buildHPA(ag)
	require.NoError(t, r.applyHPA(context.Background(), desired))

	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &hpa))
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(10), hpa.Spec.MaxReplicas)

	changed := desired.DeepCopy()
	changed.Spec.MaxReplicas = 20
	require.NoError(t, r.applyHPA(context.Background(), changed))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: desired.Name}, &hpa))
	assert.Equal(t, int32(20), hpa.Spec.MaxReplicas)
}

// TestApplyRunnerGroup_CreateThenPatch verifies the RunnerGroup CR is created
// with the desired spec/labels and a re-apply patches a changed spec field.
func TestApplyRunnerGroup_CreateThenPatch(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"standard"}}
	name := runnerGroupName(ag, spec, 0)
	desired := buildRunnerGroup(ag, spec, name)
	require.NoError(t, r.applyRunnerGroup(context.Background(), desired))

	var rg agcv1alpha1.RunnerGroup
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: name}, &rg))
	assert.Equal(t, managedLabels(ag), rg.Labels)
	assert.Equal(t, []string{"standard"}, rg.Spec.RunnerLabels)

	changedSpec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"standard"}, MaxListeners: 5}
	desired2 := buildRunnerGroup(ag, changedSpec, name)
	require.NoError(t, r.applyRunnerGroup(context.Background(), desired2))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: name}, &rg))
	assert.Equal(t, int32(5), rg.Spec.MaxListeners)
}
