package controller

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newSidecarMetrics builds a Metrics carrying only the reap-blocking-sidecar gauge,
// unregistered, so the test can read it with testutil without touching the global
// controller-runtime registry.
func newSidecarMetrics() *listener.Metrics {
	return &listener.Metrics{
		ReapBlockingSidecarTemplates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_reap_blocking_sidecar_templates",
			Help: "test",
		}, []string{"namespace", "runner_set"}),
	}
}

func sidecarTemplate(annotations map[string]string, containers ...corev1.Container) *v2alpha1.RunnerTemplateSpec {
	return &v2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Spec:       corev1.PodSpec{Containers: containers},
		},
	}
}

func TestSetReapBlockingSidecarStatus(t *testing.T) {
	runner := corev1.Container{Name: "runner"}
	dind := corev1.Container{Name: "dind"}

	t.Run("regular sidecar sets condition True and gauge to the count", func(t *testing.T) {
		m := newSidecarMetrics()
		r := &RunnerSetReconciler{Metrics: m}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner, dind), nil)

		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v2alpha1.ReasonReapBlockingSidecar, cond.Reason)
		assert.Contains(t, cond.Message, "dind")
		assert.Equal(t, 1.0, testutil.ToFloat64(m.ReapBlockingSidecarTemplates.WithLabelValues("ns", "set")))
	})

	t.Run("no sidecar sets condition False and gauge to zero", func(t *testing.T) {
		m := newSidecarMetrics()
		r := &RunnerSetReconciler{Metrics: m}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner), nil)

		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v2alpha1.ReasonNoReapBlockingSidecar, cond.Reason)
		assert.Equal(t, 0.0, testutil.ToFloat64(m.ReapBlockingSidecarTemplates.WithLabelValues("ns", "set")))
	})

	t.Run("opt-out annotation clears the condition and gauge", func(t *testing.T) {
		m := newSidecarMetrics()
		r := &RunnerSetReconciler{Metrics: m}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		acked := map[string]string{v2alpha1.SelfExitingSidecarsAnnotation: "dind"}
		r.setReapBlockingSidecarStatus(rs, sidecarTemplate(acked, runner, dind), acked)

		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, 0.0, testutil.ToFloat64(m.ReapBlockingSidecarTemplates.WithLabelValues("ns", "set")))
	})

	t.Run("nil template (unresolved) clears a previously-set condition and gauge", func(t *testing.T) {
		m := newSidecarMetrics()
		r := &RunnerSetReconciler{Metrics: m}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

		// First a reap-blocking sidecar is present…
		r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner, dind), nil)
		require.Equal(t, 1.0, testutil.ToFloat64(m.ReapBlockingSidecarTemplates.WithLabelValues("ns", "set")))

		// …then the template stops resolving: both outlets clear.
		r.setReapBlockingSidecarStatus(rs, nil, nil)
		cond := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionPossibleReapBlockingSidecar)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, 0.0, testutil.ToFloat64(m.ReapBlockingSidecarTemplates.WithLabelValues("ns", "set")))
	})

	t.Run("nil Metrics does not panic", func(t *testing.T) {
		r := &RunnerSetReconciler{}
		rs := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
		require.NotPanics(t, func() {
			r.setReapBlockingSidecarStatus(rs, sidecarTemplate(nil, runner, dind), nil)
		})
	})
}
