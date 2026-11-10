package controller

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
)

func ptrInt32(v int32) *int32 { return &v }

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

// poolResources returns the proxy pool's per-replica resources: the secure defaults,
// overlaid with overrides when given.
func poolResources(overrides *corev1.ResourceRequirements) corev1.ResourceRequirements {
	ep := &gmcv2.EgressProxy{}
	if overrides != nil {
		ep.Spec.Resources = *overrides
	}
	return egressProxyResources(ep)
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
		ObjectMeta: metav1.ObjectMeta{Name: "pool-proxy", Namespace: "tenant-ns"},
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

func quotaReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
}

func TestFootprintFromResources(t *testing.T) {
	// Defaults: 10m/32Mi requests, 500m/64Mi limits.
	fp := footprintFromResources(poolResources(nil), 10)

	pods := fp[corev1.ResourcePods]
	assert.Equal(t, int64(10), pods.Value())
	reqCPU := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, "100m", reqCPU.String(), "10 × 10m")
	reqMem := fp[corev1.ResourceRequestsMemory]
	assert.Equal(t, int64(10*32*1024*1024), reqMem.Value(), "10 × 32Mi")
	limCPU := fp[corev1.ResourceLimitsCPU]
	assert.Equal(t, int64(5), limCPU.Value(), "10 × 500m = 5 cores")

	// Negative/zero replica counts clamp to an empty (zero-pod) footprint.
	zero := footprintFromResources(poolResources(nil), -3)
	z := zero[corev1.ResourcePods]
	assert.Equal(t, int64(0), z.Value())
}

// A bring-your-own autoscaler sets the replica count the quota math multiplies by,
// up to the int32 maximum, so the product must not cost one step per replica: the
// old repeated addition ran this case past any test timeout (Q1324).
func TestFootprintFromResources_MaxInt32ReplicasIsExact(t *testing.T) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Ei")},
	}
	fp := footprintFromResources(res, math.MaxInt32)

	cpu := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(10*math.MaxInt32), cpu.MilliValue())
	mem := fp[corev1.ResourceLimitsMemory]
	assert.Equal(t, "8589934588Ei", mem.String(), "memory product past int64 must stay exact")
}

func TestFootprintFromResources_HonoursResourceOverride(t *testing.T) {
	override := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}
	fp := footprintFromResources(poolResources(override), 3)
	reqCPU := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, int64(3), reqCPU.Value(), "3 × 1 core override")
}

// evalProxyPoolQuota: the warning (pressure) tier is headroom-based and predictive.
func TestEvalProxyPoolQuota_Pressure(t *testing.T) {
	tests := []struct {
		name        string
		maxReplicas int32
		dep         *appsv1.Deployment
		quotas      []client.Object
		wantPress   bool
		wantReason  string
		wantInMsg   string
	}{
		{
			name:        "no quota in namespace",
			maxReplicas: 10,
			dep:         proxyDep(2, ""),
			wantReason:  "NoQuota",
		},
		{
			name:        "ample headroom to reach max",
			maxReplicas: 10,
			dep:         proxyDep(2, ""),
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")})},
			wantReason: "QuotaHeadroomSufficient",
		},
		{
			name:        "headroom too small to grow from current to max",
			maxReplicas: 10,
			dep:         proxyDep(2, ""),
			// hard 5 pods, 4 already used → 1 free, but reaching 10 from 2 needs 8 more.
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("4")})},
			wantPress:  true,
			wantReason: "InsufficientQuotaHeadroom",
			wantInMsg:  "pods",
		},
		{
			name:        "already at max needs no additional headroom",
			maxReplicas: 3,
			dep:         proxyDep(3, ""),
			quotas: []client.Object{quota("compute",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")})},
			wantReason: "QuotaHeadroomSufficient",
		},
		{
			name:        "cpu headroom insufficient",
			maxReplicas: 10,
			dep:         proxyDep(0, ""),
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
			st := evalProxyPoolQuota(context.Background(), quotaReader(tc.quotas...), "tenant-ns",
				tc.maxReplicas, poolResources(nil), tc.dep)
			assert.Equal(t, tc.wantPress, st.pressure)
			assert.Equal(t, tc.wantReason, st.pressureReason)
			if tc.wantInMsg != "" {
				assert.Contains(t, st.pressureMessage, tc.wantInMsg)
			}
			assert.False(t, st.exceeded, "no ReplicaFailure → not exceeded")
		})
	}
}

// evalProxyPoolQuota: the error (exceeded) tier reads the Deployment ReplicaFailure
// condition and supersedes the warning.
func TestEvalProxyPoolQuota_ExceededSupersedesPressure(t *testing.T) {
	// Quota so tight the headroom check would also flag pressure...
	tight := quota("compute",
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
		corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")})
	dep := proxyDep(3, `pods "pool-proxy-abc" is forbidden: exceeded quota: compute, requested: pods=1, used: pods=3, limited: pods=3`)

	st := evalProxyPoolQuota(context.Background(), quotaReader(tight), "tenant-ns", 10, poolResources(nil), dep)

	assert.True(t, st.exceeded)
	assert.Equal(t, "ReplicasRejected", st.exceededReason)
	assert.Contains(t, st.exceededMessage, "exceeded quota")
	// Mutually exclusive: the warning is suppressed while exceeded fires.
	assert.False(t, st.pressure, "exceeded must supersede pressure")
	assert.Equal(t, "Superseded", st.pressureReason)
}

// A non-quota ReplicaFailure (e.g. PSA, image) must NOT trip ProxyQuotaExceeded.
func TestEvalProxyPoolQuota_NonQuotaReplicaFailureIgnored(t *testing.T) {
	dep := proxyDep(0, `pods "pool-proxy-abc" is forbidden: violates PodSecurity "restricted:latest"`)
	st := evalProxyPoolQuota(context.Background(), quotaReader(), "tenant-ns", 10, poolResources(nil), dep)
	assert.False(t, st.exceeded, "non-quota ReplicaFailure must not set ProxyQuotaExceeded")
}

// A failed quota read must not assert pressure on incomplete data.
func TestEvalProxyPoolQuota_ListErrorIsBenign(t *testing.T) {
	reader := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("forbidden: no resourcequotas read access")
			},
		}).
		Build()
	st := evalProxyPoolQuota(context.Background(), reader, "tenant-ns", 10, poolResources(nil), proxyDep(2, ""))
	assert.False(t, st.pressure, "must not assert pressure when the quota read fails")
	assert.Equal(t, "QuotaUnknown", st.pressureReason)
}
