package provisioner

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// quotaTestScheme registers the RunnerGroup API types AND core/v1 so the fake
// client can serve ResourceQuota reads.
func quotaTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// quotaRunnerGroup builds a RunnerGroup whose worker container asks for cpu, or —
// when cpu is empty — declares no resources at all (so the provisioner's
// gap-fill defaults apply).
func quotaRunnerGroup(cpu string) *v1alpha1.RunnerGroup {
	c := corev1.Container{Name: WorkerContainerName}
	if cpu != "" {
		c.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		}
	}
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxWorkers: ptr.To(int32(10)),
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
			},
		},
	}
}

// cpuQuota builds a namespace ResourceQuota with the given hard/used requests.cpu.
func cpuQuota(hard, used string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "ns"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)}},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)},
			Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(used)},
		},
	}
}

// TestWorkerFootprint_GapFillsDefaults verifies the footprint sizes a container
// that declares no resources at the defaults the provisioner stamps at pod-build
// time (500m/1Gi), rather than reading it as free. Without this the quota rung is
// blind to exactly the templates most deployments run.
func TestWorkerFootprint_GapFillsDefaults(t *testing.T) {
	fp := WorkerFootprint(&corev1.PodSpec{Containers: []corev1.Container{{Name: WorkerContainerName}}}, 2)

	cpu := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, "1", cpu.String(), "two default workers request 2x500m of cpu")
	mem := fp[corev1.ResourceLimitsMemory]
	assert.Equal(t, "2Gi", mem.String(), "two default workers cap at 2x1Gi of memory")
	pods := fp[corev1.ResourcePods]
	assert.Equal(t, "2", pods.String())

	// A container that declares either requests or limits keeps the tenant's values.
	explicit := WorkerFootprint(&corev1.PodSpec{Containers: []corev1.Container{{
		Name:      WorkerContainerName,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
	}}}, 2)
	cpu = explicit[corev1.ResourceRequestsCPU]
	assert.Equal(t, "200m", cpu.String())
	_, hasMem := explicit[corev1.ResourceLimitsMemory]
	assert.False(t, hasMem, "an explicit-requests container must not inherit the default limits")
}

func TestWorkerQuotaExhausted(t *testing.T) {
	tests := []struct {
		name          string
		cpu           string
		objs          []runtime.Object
		wantExhausted bool
	}{
		{
			name:          "headroom for another worker",
			cpu:           "500m",
			objs:          []runtime.Object{cpuQuota("2", "1")},
			wantExhausted: false,
		},
		{
			name:          "exactly enough headroom still admits",
			cpu:           "500m",
			objs:          []runtime.Object{cpuQuota("2", "1500m")},
			wantExhausted: false,
		},
		{
			name:          "one millicore short",
			cpu:           "500m",
			objs:          []runtime.Object{cpuQuota("2", "1501m")},
			wantExhausted: true,
		},
		{
			name:          "no namespace quota is not exhaustion",
			cpu:           "500m",
			wantExhausted: false,
		},
		{
			name:          "defaults are charged against the quota",
			cpu:           "", // no declared resources ⇒ 500m gap-fill
			objs:          []runtime.Object{cpuQuota("2", "1900m")},
			wantExhausted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).WithRuntimeObjects(tt.objs...).Build()
			rg := quotaRunnerGroup(tt.cpu)

			exhausted, detail := WorkerQuotaExhausted(context.Background(), fc, "ns", &rg.Spec.PodTemplate.Spec)
			assert.Equal(t, tt.wantExhausted, exhausted)
			if tt.wantExhausted {
				assert.Contains(t, detail, "cannot admit another worker pod", "the detail must name the binding resource")
				assert.Contains(t, detail, "requests.cpu")
			} else {
				assert.Empty(t, detail)
			}
		})
	}
}

// TestWorkerQuotaExhausted_FailsOpenOnReadError verifies the documented fail-open
// contract: a quota the AGC cannot list must leave the gate exactly as it was, with
// createPodWithQuotaRetry remaining the backstop. The scheme here omits core/v1, so
// the List fails the way a lost cache or a missing RBAC rule would.
func TestWorkerQuotaExhausted_FailsOpenOnReadError(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(admissionTestScheme()).Build()

	exhausted, detail := WorkerQuotaExhausted(context.Background(), fc, "ns",
		&quotaRunnerGroup("500m").Spec.PodTemplate.Spec)

	assert.False(t, exhausted, "an unreadable quota must not close the gate")
	assert.Empty(t, detail)
}

// TestAdmit_QuotaRungClosesGate verifies the #784 behaviour end to end on the v1
// Target: with the namespace quota exhausted the AdmitFunc refuses with
// reason=quota, and — critically — reserves nothing, so the ceiling budget is
// untouched and recovers the moment headroom returns.
func TestAdmit_QuotaRungClosesGate(t *testing.T) {
	rg := quotaRunnerGroup("500m")
	fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
		WithObjects(rg, cpuQuota("2", "2")).Build()
	p := NewProvisioner(fc, nil, nil)

	release, ok, reason := p.AdmitFor(rg)(context.Background())
	require.False(t, ok, "an exhausted namespace quota must close the admission gate")
	assert.Equal(t, runnercore.AdmitReasonQuota, reason)
	assert.Nil(t, release)
	assert.Zero(t, p.admission.reservedCount("ns/g"), "a quota refusal must not consume a ceiling slot")

	// Headroom returns (a sibling job finished): the same gate admits again, with no
	// restart and no reset — the signal is self-clearing.
	freed := cpuQuota("2", "1")
	require.NoError(t, fc.Update(context.Background(), freed))

	release, ok, reason = p.AdmitFor(rg)(context.Background())
	require.True(t, ok, "freed quota headroom must reopen the gate")
	assert.Empty(t, reason)
	require.NotNil(t, release)
	release(runnercore.AdmitProvisioned)
}

// TestAdmit_QuotaRungOptOut verifies AGC_QUOTA_ADMISSION=false (DisableQuotaAdmission)
// restores the pre-#784 behaviour: the gate ignores quota entirely and admits on the
// declared ceiling alone.
func TestAdmit_QuotaRungOptOut(t *testing.T) {
	rg := quotaRunnerGroup("500m")
	fc := fake.NewClientBuilder().WithScheme(quotaTestScheme()).
		WithObjects(rg, cpuQuota("2", "2")).Build()
	p := NewProvisioner(fc, nil, nil)
	p.DisableQuotaAdmission = true

	release, ok, reason := p.AdmitFor(rg)(context.Background())
	require.True(t, ok, "the opt-out must ignore quota headroom")
	assert.Empty(t, reason)
	require.NotNil(t, release)
	release(runnercore.AdmitProvisioned)
}
