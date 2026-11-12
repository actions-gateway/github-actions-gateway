package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseWorkerSeparation(t *testing.T) {
	for in, want := range map[string]WorkerSeparation{
		"":         WorkerSeparationRequired,
		"required": WorkerSeparationRequired,
		"none":     WorkerSeparationNone,
	} {
		got, err := ParseWorkerSeparation(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"Required", "off", "false", "preferred"} {
		_, err := ParseWorkerSeparation(in)
		assert.Error(t, err, in)
	}
}

// The selector must match a worker pod and neither controller pod. The worker label
// set mirrors what cmd/agc/internal/provisioner buildPod stamps.
func TestWorkerPodSelector_MatchesWorkersOnly(t *testing.T) {
	sel, err := metav1.LabelSelectorAsSelector(workerPodSelector())
	require.NoError(t, err)

	worker := apilabels.Recommended("actions-runner", "rs", "runner", "2.330.0", "actions-gateway-agc")
	worker["actions-gateway/component"] = "workload"
	worker["actions-gateway/plan-id"] = "abc"
	assert.True(t, sel.Matches(labels.Set(worker)), "worker pod labels")

	agc := buildAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), "agc:test", nil, "baseline", nil)
	assert.Equal(t, componentWorkload, agc.Spec.Template.Labels[labelComponent], "the AGC shares the component label")
	assert.False(t, sel.Matches(labels.Set(agc.Spec.Template.Labels)), "AGC pod labels")

	proxy := buildEgressProxyDeployment(newEP("shared", "team-a", nil), "proxy:test", nil)
	assert.False(t, sel.Matches(labels.Set(proxy.Spec.Template.Labels)), "proxy pod labels")
}

func requiredAntiAffinity(dep *appsv1.Deployment) []corev1.PodAffinityTerm {
	a := dep.Spec.Template.Spec.Affinity
	if a == nil || a.PodAntiAffinity == nil {
		return nil
	}
	return a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
}

func hasWorkerSeparationTerm(dep *appsv1.Deployment) bool {
	want := workerSeparationTerm()
	for _, term := range requiredAntiAffinity(dep) {
		if assert.ObjectsAreEqual(want, term) {
			return true
		}
	}
	return false
}

func TestWorkerSeparationTerm_AllNamespacesOnHostname(t *testing.T) {
	term := workerSeparationTerm()
	require.NotNil(t, term.NamespaceSelector, "a nil namespaceSelector scopes the term to the controller's own namespace")
	assert.Empty(t, term.NamespaceSelector.MatchLabels)
	assert.Empty(t, term.NamespaceSelector.MatchExpressions)
	assert.Equal(t, corev1.LabelHostname, term.TopologyKey)
}

func TestActionsGatewayV2Reconcile_WorkerSeparation(t *testing.T) {
	tenantTerm := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
		TopologyKey:   corev1.LabelTopologyZone,
	}
	for _, tc := range []struct {
		name       string
		mode       WorkerSeparation
		scheduling *gmcv2.PodScheduling
		wantTerm   bool
		wantTerms  int
	}{
		{name: "zero value is required", wantTerm: true, wantTerms: 1},
		{name: "required", mode: WorkerSeparationRequired, wantTerm: true, wantTerms: 1},
		{name: "none", mode: WorkerSeparationNone},
		{
			name: "a tenant anti-affinity cannot drop it",
			scheduling: &gmcv2.PodScheduling{Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{tenantTerm},
			}}},
			wantTerm: true, wantTerms: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := actionsGatewayV2TestScheme(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
			proxy := &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"}}
			ag := v2Gateway("gw", "team-a", "github-app", "shared")
			ag.Spec.Scheduling = tc.scheduling
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, secret, proxy, ag).WithStatusSubresource(ag).Build()

			r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", WorkerSeparation: tc.mode}
			reconcileV2Gateway(t, r, "team-a", "gw")

			var dep appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
			assert.Equal(t, tc.wantTerm, hasWorkerSeparationTerm(&dep))
			assert.Len(t, requiredAntiAffinity(&dep), tc.wantTerms)
		})
	}
}

func TestEgressProxyReconcile_WorkerSeparation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       WorkerSeparation
		scheduling *gmcv2.PodScheduling
		wantTerm   bool
		wantTerms  int
	}{
		{name: "zero value keeps the spread and adds the term", wantTerm: true, wantTerms: 2},
		{name: "none keeps only the spread", mode: WorkerSeparationNone, wantTerms: 1},
		{
			name:       "an author's empty anti-affinity drops the spread, not the term",
			scheduling: &gmcv2.PodScheduling{Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}},
			wantTerm:   true, wantTerms: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := egressProxyTestScheme(t)
			ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) { ep.Spec.Scheduling = tc.scheduling })
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).WithStatusSubresource(ep).Build()

			r := &EgressProxyReconciler{Client: c, Scheme: scheme, IPCache: &IPRangeCache{}, ProxyImage: "proxy:test", WorkerSeparation: tc.mode}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
			require.NoError(t, err)

			var dep appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}, &dep))
			assert.Equal(t, tc.wantTerm, hasWorkerSeparationTerm(&dep))
			assert.Len(t, requiredAntiAffinity(&dep), tc.wantTerms)
		})
	}
}
