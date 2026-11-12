//go:build integration

package integration_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// assertWorkerSeparation checks the stored Deployment still carries the required
// anti-affinity against worker pods after the apiserver round-trip. The reconcilers
// in this suite leave WorkerSeparation at its zero value, which is required.
func assertWorkerSeparation(t *testing.T, dep *appsv1.Deployment) {
	t.Helper()
	a := dep.Spec.Template.Spec.Affinity
	if !assert.NotNil(t, a, "pod affinity") || !assert.NotNil(t, a.PodAntiAffinity, "pod anti-affinity") {
		return
	}
	for _, term := range a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if !isWorkerSeparationTerm(term) {
			continue
		}
		assert.Equal(t, map[string]string{
			"actions-gateway/component":   "workload",
			"app.kubernetes.io/component": "runner",
		}, term.LabelSelector.MatchLabels)
		if assert.NotNil(t, term.NamespaceSelector, "a nil namespaceSelector would scope the term to the controller's namespace") {
			assert.Empty(t, term.NamespaceSelector.MatchLabels)
			assert.Empty(t, term.NamespaceSelector.MatchExpressions)
		}
		assert.Equal(t, corev1.LabelHostname, term.TopologyKey)
		return
	}
	t.Errorf("Deployment %s/%s has no required anti-affinity against worker pods", dep.Namespace, dep.Name)
}

func isWorkerSeparationTerm(term corev1.PodAffinityTerm) bool {
	return term.LabelSelector != nil && term.LabelSelector.MatchLabels["app.kubernetes.io/component"] == "runner"
}

// spreadTerms returns the required anti-affinity terms other than the worker
// separation term, which the reconcilers append to every proxy pod.
func spreadTerms(terms []corev1.PodAffinityTerm) []corev1.PodAffinityTerm {
	var out []corev1.PodAffinityTerm
	for _, term := range terms {
		if !isWorkerSeparationTerm(term) {
			out = append(out, term)
		}
	}
	return out
}
