package v1alpha1

import (
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// TestListenerVocabularyParityWithV2 pins the cross-package value parity the
// shared classic listener relies on (Q309): the listener goroutines
// (internal/listener) push session-failure conditions using this package's
// constants onto both the v1 RunnerGroup and the v2 RunnerSet, and the v2
// packages declare same-value constants so the v2 vocabulary is complete. If a
// value drifts, the condition a v2 RunnerSet actually carries would no longer
// match the documented v2 vocabulary.
func TestListenerVocabularyParityWithV2(t *testing.T) {
	pairs := []struct {
		name            string
		v1, alpha, beta string
	}{
		{"ConditionDegraded", ConditionDegraded, v2alpha1.ConditionDegraded, v2beta1.ConditionDegraded},
		{"ConditionRateLimited", ConditionRateLimited, v2alpha1.ConditionRateLimited, v2beta1.ConditionRateLimited},
		{"ConditionRunnerVersionTooOld", ConditionRunnerVersionTooOld, v2alpha1.ConditionRunnerVersionTooOld, v2beta1.ConditionRunnerVersionTooOld},
		{"ReasonSustainedRateLimit", ReasonSustainedRateLimit, v2alpha1.ReasonSustainedRateLimit, v2beta1.ReasonSustainedRateLimit},
		{"ReasonVersionTooOld", ReasonVersionTooOld, v2alpha1.ReasonVersionTooOld, v2beta1.ReasonVersionTooOld},
		{"ReasonWorkerImageBelowMinimum", ReasonWorkerImageBelowMinimum, v2alpha1.ReasonWorkerImageBelowMinimum, v2beta1.ReasonWorkerImageBelowMinimum},
		{"ReasonWorkerImageCurrent", ReasonWorkerImageCurrent, v2alpha1.ReasonWorkerImageCurrent, v2beta1.ReasonWorkerImageCurrent},
		{"ReasonWorkerImageVersionUnknown", ReasonWorkerImageVersionUnknown, v2alpha1.ReasonWorkerImageVersionUnknown, v2beta1.ReasonWorkerImageVersionUnknown},
		{"ReasonSessionUnauthorized", ReasonSessionUnauthorized, v2alpha1.ReasonSessionUnauthorized, v2beta1.ReasonSessionUnauthorized},
		{"ReasonPollingHealthy", ReasonPollingHealthy, v2alpha1.ReasonPollingHealthy, v2beta1.ReasonPollingHealthy},
		{"ReasonSessionAuthorized", ReasonSessionAuthorized, v2alpha1.ReasonSessionAuthorized, v2beta1.ReasonSessionAuthorized},
	}
	for _, p := range pairs {
		if p.v1 != p.alpha || p.v1 != p.beta {
			t.Errorf("%s diverged: v1alpha1=%q v2alpha1=%q v2beta1=%q", p.name, p.v1, p.alpha, p.beta)
		}
	}
}
