package controller

import (
	"context"
	"errors"
	"testing"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ptrInt32(v int32) *int32 { return &v }

// agWithProxy returns an ActionsGateway whose proxy pool has the given
// maxReplicas and optional per-replica resource overrides.
func agWithProxy(maxReplicas int32, overrides *corev1.ResourceRequirements) *gmcv1alpha1.ActionsGateway {
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "tenant-ns"},
	}
	ag.Spec.Proxy.MaxReplicas = ptrInt32(maxReplicas)
	if overrides != nil {
		ag.Spec.Proxy.Resources = *overrides
	}
	return ag
}

// quota builds a ResourceQuota with the given hard caps and current usage (both
// reflected in .status, where the controller reads them).
func quota(name string, hard, used corev1.ResourceList) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-ns"},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
		Status:     corev1.ResourceQuotaStatus{Hard: hard, Used: used},
	}
}

// proxyDep builds a proxy Deployment with the given current replica count and an
// optional ReplicaFailure condition message.
func proxyDep(currentReplicas int32, replicaFailureMsg string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: "tenant-ns"},
		Status:     appsv1.DeploymentStatus{Replicas: currentReplicas},
	}
	if replicaFailureMsg != "" {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:    appsv1.DeploymentReplicaFailure,
			Status:  corev1.ConditionTrue,
			Reason:  "FailedCreate",
			Message: replicaFailureMsg,
		}}
	}
	return d
}

func TestProxyFootprint(t *testing.T) {
	// Defaults: 10m/32Mi requests, 500m/64Mi limits.
	fp := proxyFootprint(agWithProxy(10, nil), 10)

	pods := fp[corev1.ResourcePods]
	assert.Equal(t, int64(10), pods.Value())
	reqCPU := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, "100m", reqCPU.String(), "10 × 10m")
	reqMem := fp[corev1.ResourceRequestsMemory]
	assert.Equal(t, int64(10*32*1024*1024), reqMem.Value(), "10 × 32Mi")
	limCPU := fp[corev1.ResourceLimitsCPU]
	assert.Equal(t, int64(5), limCPU.Value(), "10 × 500m = 5 cores")

	// Negative/zero replica counts clamp to an empty (zero-pod) footprint.
	zero := proxyFootprint(agWithProxy(10, nil), -3)
	z := zero[corev1.ResourcePods]
	assert.Equal(t, int64(0), z.Value())
}

func TestProxyFootprint_HonoursResourceOverride(t *testing.T) {
	override := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}
	fp := proxyFootprint(agWithProxy(3, override), 3)
	reqCPU := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(3), reqCPU.Value(), "3 × 1 core override")
}

// evalProxyQuota: the warning (pressure) tier is headroom-based and predictive.
func TestEvalProxyQuota_Pressure(t *testing.T) {
	tests := []struct {
		name       string
		ag         *gmcv1alpha1.ActionsGateway
		dep        *appsv1.Deployment
		quotas     []client.Object
		wantPress  bool
		wantReason string
		wantInMsg  string
	}{
		{
			name:       "no quota in namespace",
			ag:         agWithProxy(10, nil),
			dep:        proxyDep(2, ""),
			wantReason: "NoQuota",
		},
		{
			name: "ample headroom to reach max",
			ag:   agWithProxy(10, nil),
			dep:  proxyDep(2, ""),
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")})},
			wantReason: "QuotaHeadroomSufficient",
		},
		{
			name: "headroom too small to grow from current to max",
			ag:   agWithProxy(10, nil),
			dep:  proxyDep(2, ""),
			// hard 5 pods, 4 already used → 1 free, but reaching 10 from 2 needs 8 more.
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("4")})},
			wantPress:  true,
			wantReason: "InsufficientQuotaHeadroom",
			wantInMsg:  "pods",
		},
		{
			name: "already at max needs no additional headroom",
			ag:   agWithProxy(3, nil),
			dep:  proxyDep(3, ""),
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")})},
			wantReason: "QuotaHeadroomSufficient",
		},
		{
			name: "cpu headroom insufficient",
			ag:   agWithProxy(10, nil),
			dep:  proxyDep(0, ""),
			// 10 × 10m = 100m needed, hard 50m, none used.
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("50m")}, nil)},
			wantPress:  true,
			wantReason: "InsufficientQuotaHeadroom",
			wantInMsg:  "requests.cpu",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &ActionsGatewayReconciler{
				Client: fake.NewClientBuilder().WithScheme(statusTestScheme(t)).WithObjects(tc.quotas...).Build(),
			}
			st := r.evalProxyQuota(context.Background(), tc.ag, tc.dep)
			assert.Equal(t, tc.wantPress, st.pressure)
			assert.Equal(t, tc.wantReason, st.pressureReason)
			if tc.wantInMsg != "" {
				assert.Contains(t, st.pressureMessage, tc.wantInMsg)
			}
			assert.False(t, st.exceeded, "no ReplicaFailure → not exceeded")
		})
	}
}

// evalProxyQuota: the error (exceeded) tier reads the Deployment ReplicaFailure
// condition and supersedes the warning.
func TestEvalProxyQuota_ExceededSupersedesPressure(t *testing.T) {
	ag := agWithProxy(10, nil)
	// Quota so tight the headroom check would also flag pressure...
	tight := quota("compute",
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")})
	dep := proxyDep(3, `pods "actions-gateway-proxy-abc" is forbidden: exceeded quota: compute, requested: pods=1, used: pods=3, limited: pods=3`)

	r := &ActionsGatewayReconciler{
		Client: fake.NewClientBuilder().WithScheme(statusTestScheme(t)).WithObjects(tight).Build(),
	}
	st := r.evalProxyQuota(context.Background(), ag, dep)

	assert.True(t, st.exceeded)
	assert.Equal(t, "ReplicasRejected", st.exceededReason)
	assert.Contains(t, st.exceededMessage, "exceeded quota")
	// Mutually exclusive: the warning is suppressed while exceeded fires.
	assert.False(t, st.pressure, "exceeded must supersede pressure")
	assert.Equal(t, "Superseded", st.pressureReason)
}

// A non-quota ReplicaFailure (e.g. PSA, image) must NOT trip ProxyQuotaExceeded.
func TestEvalProxyQuota_NonQuotaReplicaFailureIgnored(t *testing.T) {
	ag := agWithProxy(10, nil)
	dep := proxyDep(0, `pods "actions-gateway-proxy-abc" is forbidden: violates PodSecurity "restricted:latest"`)
	r := &ActionsGatewayReconciler{
		Client: fake.NewClientBuilder().WithScheme(statusTestScheme(t)).Build(),
	}
	st := r.evalProxyQuota(context.Background(), ag, dep)
	assert.False(t, st.exceeded, "non-quota ReplicaFailure must not set ProxyQuotaExceeded")
}

// A failed quota read must not assert pressure on incomplete data.
func TestEvalProxyQuota_ListErrorIsBenign(t *testing.T) {
	r := &ActionsGatewayReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(statusTestScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("forbidden: no resourcequotas read access")
				},
			}).
			Build(),
	}
	st := r.evalProxyQuota(context.Background(), agWithProxy(10, nil), proxyDep(2, ""))
	assert.False(t, st.pressure, "must not assert pressure when the quota read fails")
	assert.Equal(t, "QuotaUnknown", st.pressureReason)
}

// updateStatus publishes both conditions and does not let either gate Ready.
func TestUpdateStatus_SetsProxyQuotaConditions(t *testing.T) {
	ag := agWithProxy(10, nil)
	tightQuota := quota("compute",
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")})

	r := &ActionsGatewayReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(statusTestScheme(t)).
			WithObjects(ag, tightQuota).
			WithStatusSubresource(ag).
			Build(),
	}

	_, err := r.updateStatus(context.Background(), ag)
	require.NoError(t, err)

	pressure := meta.FindStatusCondition(ag.Status.Conditions, "ProxyQuotaPressure")
	require.NotNil(t, pressure)
	assert.Equal(t, metav1.ConditionTrue, pressure.Status)
	assert.Equal(t, "InsufficientQuotaHeadroom", pressure.Reason)

	exceeded := meta.FindStatusCondition(ag.Status.Conditions, "ProxyQuotaExceeded")
	require.NotNil(t, exceeded, "ProxyQuotaExceeded must always be published")
	assert.Equal(t, metav1.ConditionFalse, exceeded.Status)
}

func TestResourceListEqual(t *testing.T) {
	a := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}
	b := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5000m")}
	assert.True(t, resourceListEqual(a, b), "equal value, different canonical form")

	c := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("6")}
	assert.False(t, resourceListEqual(a, c))

	d := corev1.ResourceList{
		corev1.ResourcePods:        resource.MustParse("5"),
		corev1.ResourceRequestsCPU: resource.MustParse("1"),
	}
	assert.False(t, resourceListEqual(a, d), "differing key sets")
}

func TestConditionGaugeValue(t *testing.T) {
	conds := []metav1.Condition{
		{Type: "ProxyQuotaPressure", Status: metav1.ConditionTrue},
		{Type: "ProxyQuotaExceeded", Status: metav1.ConditionFalse},
	}
	assert.Equal(t, float64(1), conditionGaugeValue(conds, "ProxyQuotaPressure"))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, "ProxyQuotaExceeded"))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, "Absent"))
}
