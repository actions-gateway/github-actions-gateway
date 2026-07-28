package controller

import (
	"fmt"
	"strings"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func gateway(name string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// TestRunnerGroupNameIsAValidLabelValue is the regression this bound exists for.
// The derived name is not only a CR name (253 chars) — the AGC stamps it as the
// actions-gateway/runner-group LABEL VALUE on every worker pod and agent Secret,
// and label values stop at 63. An unbounded name is therefore accepted by the
// apiserver here and rejected there, so the RunnerGroup reconciles happily while
// every worker pod create fails and the tenant runs no jobs at all — the same
// misleading "runner lost communication" symptom as Q467, one level up.
//
// A 15-character gateway with a 40-character runner label was enough to overrun it.
func TestRunnerGroupNameIsAValidLabelValue(t *testing.T) {
	for _, gwLen := range []int{1, 9, 14, 15, 20, 30, 52, 63, 200} {
		for _, labelLen := range []int{1, 11, 20, 36, 40, 60, 200} {
			t.Run(fmt.Sprintf("gw-%d-label-%d", gwLen, labelLen), func(t *testing.T) {
				ag := gateway(strings.Repeat("g", gwLen))
				spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{strings.Repeat("l", labelLen)}}

				name := runnerGroupName(ag, spec, 0)
				assert.LessOrEqual(t, len(name), apinames.MaxLabelValue)
				assert.Empty(t, validation.IsValidLabelValue(name), "as a label value: %q", name)
				assert.Empty(t, validation.IsDNS1123Subdomain(name), "as a CR name: %q", name)

				// The unlabelled fallback shares the same budget.
				idx := runnerGroupName(ag, agcv1alpha1.RunnerGroupSpec{}, 7)
				assert.LessOrEqual(t, len(idx), apinames.MaxLabelValue)
				assert.Empty(t, validation.IsValidLabelValue(idx), "index fallback: %q", idx)
			})
		}
	}
}

// TestRunnerGroupNameUnchangedWhenItFits is the other half of the bound: bounding
// must not rename gateways that already work. Every derivation that was within the
// limit before keeps exactly the name it has, so adopting this changes nothing for
// a healthy tenant and renames only one that could never place a worker pod.
func TestRunnerGroupNameUnchangedWhenItFits(t *testing.T) {
	cases := []struct {
		gateway string
		label   string
	}{
		{"dogfood-migrate", "gag-migrate-v1"},
		{"dfmigrate", "self-hosted"},
		{"tenant", "gpu/a100"},
		{strings.Repeat("g", 14), strings.Repeat("l", 40)}, // exactly 63 — the last name that fits
	}
	for _, tc := range cases {
		t.Run(tc.gateway+"/"+tc.label, func(t *testing.T) {
			ag := gateway(tc.gateway)
			spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{tc.label}}
			want := tc.gateway + "-" + labelSafe(tc.label)
			require.LessOrEqual(t, len(want), apinames.MaxLabelValue, "fixture must be a fitting name")
			assert.Equal(t, want, runnerGroupName(ag, spec, 0),
				"a name that already fits must not change")
		})
	}
}

// TestRunnerGroupNameStaysUnique guards the truncation: two gateways or two labels
// that differ only past the cut must not collapse onto one RunnerGroup, which would
// have two spec entries fighting over a single CR.
func TestRunnerGroupNameStaysUnique(t *testing.T) {
	seen := map[string]string{}
	long := strings.Repeat("g", 55)
	for i := range 300 {
		ag := gateway(fmt.Sprintf("%s-%d", long, i))
		spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{strings.Repeat("l", 45)}}
		name := runnerGroupName(ag, spec, 0)
		require.NotContains(t, seen, name, "collision with gateway %q", seen[name])
		seen[name] = ag.Name
	}
	for i := range 300 {
		ag := gateway(long)
		spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{fmt.Sprintf("%s-%d", strings.Repeat("l", 45), i)}}
		name := runnerGroupName(ag, spec, 0)
		require.NotContains(t, seen, name, "collision with %q", seen[name])
		seen[name] = spec.RunnerLabels[0]
	}
}

// TestDerivedAgentPoolNamesFitTheirLimits walks the chain the RunnerGroup name feeds:
// the AGC derives the agent Secret name and the GitHub runner name from it. The
// 63-char bound above is what keeps those inside their own budgets, so they are
// asserted here rather than assumed.
func TestDerivedAgentPoolNamesFitTheirLimits(t *testing.T) {
	ag := gateway(strings.Repeat("g", 63))
	spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{strings.Repeat("l", 60)}}
	rg := runnerGroupName(ag, spec, 0)

	for _, index := range []int{0, 9, 99} {
		secret := fmt.Sprintf("agentpool-%s-%d", rg, index)      // agentpool.runnerGroupSecretName
		rsSecret := fmt.Sprintf("agentpool-rs-%s-%d", rg, index) // agentpool.runnerSetSecretName
		for _, name := range []string{secret, rsSecret} {
			assert.LessOrEqualf(t, len(name), apinames.MaxObjectName, "Secret name %q", name)
			assert.Emptyf(t, validation.IsDNS1123Subdomain(name), "Secret name %q", name)
		}
	}
}
