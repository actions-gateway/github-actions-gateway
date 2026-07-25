package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The v2 half of the #784 admission-gate quota rung: runnerSetTarget.QuotaExhausted
// must size the worker off the same resolved-template-plus-sizing shape Resolve
// builds, and fail open on every read it cannot complete.

// quotaRS builds a namespace ResourceQuota whose requests.cpu is `hard` with `used`
// already consumed.
func quotaRS(ns, hard, used string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)}},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)},
			Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(used)},
		},
	}
}

func quotaTarget(t *testing.T, ns string, objs ...client.Object) *runnerSetTarget {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build()
	return &runnerSetTarget{
		client: c,
		prov:   provisioner.NewProvisioner(c, nil, slog.Default()),
		key:    client.ObjectKey{Namespace: ns, Name: "set"},
		uid:    "uid-1",
	}
}

// TestRunnerSetTarget_QuotaExhausted covers the rung's two live outcomes. tmplObj's
// container declares no resources, so each worker is charged the provisioner's
// 500m gap-fill default.
func TestRunnerSetTarget_QuotaExhausted(t *testing.T) {
	const ns = "team-a"

	t.Run("headroom admits", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "1"))
		exhausted, detail := target.QuotaExhausted(context.Background())
		assert.False(t, exhausted)
		assert.Empty(t, detail)
	})

	t.Run("exhausted closes the gate", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "1600m"))
		exhausted, detail := target.QuotaExhausted(context.Background())
		assert.True(t, exhausted, "400m free cannot admit a 500m worker")
		assert.Contains(t, detail, "requests.cpu")
	})

	t.Run("no namespace quota", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns))
		exhausted, _ := target.QuotaExhausted(context.Background())
		assert.False(t, exhausted)
	})
}

// TestRunnerSetTarget_QuotaExhausted_FailsOpen verifies the gate never starves a set
// whose references it cannot resolve: that case must fail CLOSED in Resolve (§H.7),
// with a diagnosable TemplateNotFound, not silently as "no capacity".
func TestRunnerSetTarget_QuotaExhausted_FailsOpen(t *testing.T) {
	const ns = "team-a"

	t.Run("set missing", func(t *testing.T) {
		target := quotaTarget(t, ns, gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))
		exhausted, _ := target.QuotaExhausted(context.Background())
		assert.False(t, exhausted)
	})

	t.Run("gateway missing", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))
		exhausted, _ := target.QuotaExhausted(context.Background())
		assert.False(t, exhausted)
	})

	t.Run("template chain unresolved", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), quotaRS(ns, "2", "2"))
		exhausted, _ := target.QuotaExhausted(context.Background())
		assert.False(t, exhausted)
	})
}

// TestRunnerSetTarget_QuotaExhausted_HonoursSizingProfile proves the gate and the
// WorkerQuota conditions read the SAME pod shape: a Binpack set is charged its
// sizing-adjusted request, not the template's declared one. Without this the gate
// would contradict the condition for every set on an opt-in sizing profile.
func TestRunnerSetTarget_QuotaExhausted_HonoursSizingProfile(t *testing.T) {
	const ns = "team-a"
	tmpl := tmplObj("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}
		rs.Status.SizingRecommendation = []v2alpha1.ContainerSizingRecommendation{{
			Container:    "runner",
			Requests:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			Limits:       corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			ObservedPeak: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m"), corev1.ResourceMemory: resource.MustParse("1536Mi")},
			SampleCount:  25,
		}}
	})

	// 1 CPU of headroom: too little for the template's declared 2, ample for the
	// 250m the Binpack profile actually provisions.
	target := quotaTarget(t, ns, rs, gwObj("gw", ns, ""), tmpl, quotaRS(ns, "4", "3"))

	exhausted, detail := target.QuotaExhausted(context.Background())
	assert.False(t, exhausted, "the gate must charge the sizing-adjusted request, not the template's: %s", detail)

	// Sanity: the same shape drives the advisory condition.
	var live v2alpha1.RunnerSet
	require.NoError(t, target.client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &live))
	containers := runnerSetWorkerContainers(&live, &tmpl.Spec)
	cpu := containers[0].Resources.Requests[corev1.ResourceCPU]
	assert.Equal(t, "250m", cpu.String())
}
