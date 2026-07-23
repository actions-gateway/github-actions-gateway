package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// desiredProxySpec is a stand-in for what the proxy Deployment builders produce:
// a replica count equal to the pool's minReplicas floor, plus the selector and pod
// template the reconciler genuinely owns.
func desiredProxySpec(floor int32) appsv1.DeploymentSpec {
	return appsv1.DeploymentSpec{
		Replicas: ptr(floor),
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "proxy"}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "proxy"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "proxy:v2"}}},
		},
	}
}

// The reconciler owns the floor, the HPA owns everything above it (Q283).
func TestAssignHPATargetDeploymentSpec_Replicas(t *testing.T) {
	tests := []struct {
		name string
		live *int32
		want int32
	}{
		{name: "create seeds the floor", live: nil, want: 2},
		{name: "zero is restored to the floor (an HPA cannot scale from zero)", live: ptr(int32(0)), want: 2},
		{name: "an HPA scale-out is preserved", live: ptr(int32(5)), want: 5},
		{name: "a count below the floor is left to the HPA to raise", live: ptr(int32(1)), want: 1},
		{name: "a count at the floor is untouched", live: ptr(int32(2)), want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := appsv1.DeploymentSpec{Replicas: tc.live}
			assignHPATargetDeploymentSpec(&live, desiredProxySpec(2))
			require.NotNil(t, live.Replicas)
			assert.Equal(t, tc.want, *live.Replicas)
		})
	}
}

// With managedAutoscaling=false the external autoscaler owns replicas outright
// (Q173): only a create seeds the count, and zero is deliberately NOT restored —
// an external scaler may park the pool at zero.
func TestAssignExternallyScaledDeploymentSpec_Replicas(t *testing.T) {
	tests := []struct {
		name string
		live *int32
		want int32
	}{
		{name: "create seeds the initial count", live: nil, want: 2},
		{name: "an external scale-to-zero is preserved", live: ptr(int32(0)), want: 0},
		{name: "an external scale-out is preserved", live: ptr(int32(5)), want: 5},
		{name: "a count below the CR floor is preserved", live: ptr(int32(1)), want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := appsv1.DeploymentSpec{Replicas: tc.live}
			assignExternallyScaledDeploymentSpec(&live, desiredProxySpec(2))
			require.NotNil(t, live.Replicas)
			assert.Equal(t, tc.want, *live.Replicas)
		})
	}
}

// Fields the reconciler owns are still reconciled onto a drifted live object, and
// server-defaulted fields it does not set are left alone.
func TestAssignHPATargetDeploymentSpec_ManagedAndServerFields(t *testing.T) {
	live := appsv1.DeploymentSpec{
		Replicas: ptr(int32(5)),
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "tampered"}},
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "proxy:stale"}}},
		},
		// Server-defaulted on an existing Deployment; the builders never set these.
		RevisionHistoryLimit:    ptr(int32(10)),
		ProgressDeadlineSeconds: ptr(int32(600)),
		Strategy:                appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
	}

	assignHPATargetDeploymentSpec(&live, desiredProxySpec(2))

	assert.Equal(t, "proxy", live.Selector.MatchLabels["app"], "selector should be reconciled")
	require.Len(t, live.Template.Spec.Containers, 1)
	assert.Equal(t, "proxy:v2", live.Template.Spec.Containers[0].Image, "pod template should be reconciled")

	assert.Equal(t, int32(5), *live.Replicas, "HPA scale-out preserved")
	assert.Equal(t, int32(10), *live.RevisionHistoryLimit, "server-defaulted field preserved")
	assert.Equal(t, int32(600), *live.ProgressDeadlineSeconds, "server-defaulted field preserved")
	assert.Equal(t, appsv1.RollingUpdateDeploymentStrategyType, live.Strategy.Type, "server-defaulted field preserved")
}
