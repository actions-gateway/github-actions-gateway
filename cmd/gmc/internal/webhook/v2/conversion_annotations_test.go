package v2

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// TestConversionAnnotations_SurviveTheHub pins the annotation key this package
// repeats to the conversion that writes it: a value written at v2alpha1 reaches the
// v2 view the validators receive, through the v2beta1 hub as the apiserver routes it.
func TestConversionAnnotations_SurviveTheHub(t *testing.T) {
	t.Run("EgressProxy alias", func(t *testing.T) {
		for _, mode := range []agcv2alpha1.EgressPolicyMode{
			agcv2alpha1.EgressPolicyModeCiliumFQDN, agcv2alpha1.EgressPolicyModeCalicoFQDN,
		} {
			src := &agcv2alpha1.EgressProxy{
				ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
				Spec:       agcv2alpha1.EgressProxySpec{EgressPolicyMode: mode},
			}
			hub := &v2beta1.EgressProxy{}
			require.NoError(t, src.ConvertTo(hub))
			got := &agcv2.EgressProxy{}
			require.NoError(t, got.ConvertFrom(hub))

			assert.Equal(t, agcv2.EgressPolicyModeFQDN, got.Spec.EgressPolicyMode,
				"the v2 view cannot show the alias, which is why the validator must not read spec alone")
			assert.Equal(t, agcv2.EgressPolicyMode(mode), egressPolicyModeOf(got))
		}
	})
}

// TestEgressPolicyModeOf_AliasOnlyUnderFQDN asserts the reader follows api/v2's
// ConvertTo: an alias annotation counts only while the mode is FQDN, so a v2 write
// setting CIDR is the migration off the alias it will be stored as.
func TestEgressPolicyModeOf_AliasOnlyUnderFQDN(t *testing.T) {
	ep := func(mode agcv2.EgressPolicyMode, alias string) *agcv2.EgressProxy {
		return &agcv2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{annEgressPolicyMode: alias}},
			Spec:       agcv2.EgressProxySpec{EgressPolicyMode: mode},
		}
	}
	assert.Equal(t, egressPolicyModeCiliumFQDN, egressPolicyModeOf(ep(agcv2.EgressPolicyModeFQDN, "CiliumFQDN")))
	assert.Equal(t, agcv2.EgressPolicyModeCIDR, egressPolicyModeOf(ep(agcv2.EgressPolicyModeCIDR, "CiliumFQDN")))
	assert.Equal(t, agcv2.EgressPolicyModeFQDN, egressPolicyModeOf(ep(agcv2.EgressPolicyModeFQDN, "Bogus")))
}
