//go:build integration

package integration_test

import (
	"context"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	webhookv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// v2GatewayWithScheduling returns a v2 ActionsGateway naming priorityClassName on
// its AGC control-plane pod — one of the two infra surfaces the infra-only
// allowlist gates (Q284). An empty name leaves the scheduling block unset.
func v2GatewayWithScheduling(ns, name, priorityClassName string) *agcv2alpha1.ActionsGateway {
	ag := &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: "https://github.com/example-org"},
	}
	if priorityClassName != "" {
		ag.Spec.Scheduling = &agcv2alpha1.PodScheduling{PriorityClassName: priorityClassName}
	}
	return ag
}

// TestIntegration_InfraPriorityClassAllowlist_Watch is the Q298 test against a real
// apiserver: the infra allowlist gains the same watched, additive, fail-safe
// dynamic half the worker allowlist got in Q188 — and gaining it must not open the
// route the two separate allowlists exist to close.
//
// The negative cases carry the weight. A CR that would put one class on both lists
// is rejected by the CRD's CEL rule at write time; a CR whose infra list collides
// with the worker *flag* (which CEL cannot see) is refused wholesale by the
// reconciler; and no CR at all widens nothing.
func TestIntegration_InfraPriorityClassAllowlist_Watch(t *testing.T) {
	const (
		ns         = "gmc-q298"
		pcaName    = "gmc-q298-priority-class-allowlist"
		workerFlag = "runner-standard"
		infraFlag  = "gag-infra-critical"
		infraDyn   = "gag-infra-high"
		escalation = "system-cluster-critical"
	)

	createNamespace(t, ns)

	// The two live allowlists as the GMC builds them: seeded from the disjoint
	// static flags and paired, so a name reaching both is admitted by neither.
	worker := allowlist.New([]string{workerFlag})
	infra := allowlist.New([]string{infraFlag})
	allowlist.Pair(worker, infra)
	require.Empty(t, allowlist.Intersection(worker, infra), "precondition: the static flags are disjoint")

	gwValidator := &webhookv2alpha1.ActionsGatewayCustomValidator{InfraPriorityClasses: infra}
	startPriorityClassAllowlistReconcilerPair(t, worker, infra, pcaName)

	// No CR yet: the flag allowlist alone is in force. An unset name still passes —
	// the secure default forbids named classes, not unprioritized infra pods.
	_, err := gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "flag-ok", infraFlag))
	require.NoError(t, err, "the static infra flag class must be admitted")
	_, err = gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "unset-ok", ""))
	require.NoError(t, err, "an unset priorityClassName must stay admissible")
	_, err = gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "dyn-early", infraDyn))
	require.Error(t, err, "no CR must mean no dynamic infra additions")
	_, err = gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "escalate", escalation))
	require.Error(t, err, "%s must never be nameable on an infra pod", escalation)

	// Apply the CR: the infra list takes effect with no restart, and the worker list
	// alongside it stays on its own surface.
	pca := &v2beta1.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: pcaName},
		Spec: v2beta1.PriorityClassAllowlistSpec{
			AllowedPriorityClasses:      []string{"runner-bursty"},
			AllowedInfraPriorityClasses: []string{infraDyn},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pca))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pca) })
	waitForAllowed(t, infra, infraDyn, true)

	_, err = gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "dyn-ok", infraDyn))
	require.NoError(t, err, "the CR-sourced infra class must be admitted without a restart")
	assert.True(t, infra.Allowed(infraFlag), "the static infra flag must survive a dynamic augmentation")
	assert.False(t, worker.Allowed(infraDyn), "an infra class must not leak onto the worker allowlist")
	assert.False(t, infra.Allowed("runner-bursty"), "a worker class must not leak onto the infra allowlist")

	// Negative: one class on BOTH lists of the same object. The CRD's CEL rule
	// refuses the write, so the overlap never reaches the GMC or a running pod.
	bothLists := &v2beta1.PriorityClassAllowlist{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pcaName}, bothLists))
	bothLists.Spec.AllowedInfraPriorityClasses = []string{infraDyn, "runner-bursty"}
	err = k8sClient.Update(ctx, bothLists)
	require.Error(t, err, "a class on both CR lists must be rejected at write time")
	assert.Contains(t, err.Error(), "must be disjoint")
	assert.True(t, infra.Allowed(infraDyn), "a refused write must not disturb the live allowlists")

	// Negative: the infra list names a class the WORKER FLAG pins. CEL cannot see a
	// controller flag, so this write is admitted — and the reconciler must refuse the
	// pair wholesale, dropping both dynamic sets back to the flags.
	collide := &v2beta1.PriorityClassAllowlist{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pcaName}, collide))
	collide.Spec.AllowedInfraPriorityClasses = []string{infraDyn, workerFlag}
	require.NoError(t, k8sClient.Update(ctx, collide), "CEL cannot see the flags, so this write is admitted")

	waitForAllowed(t, infra, infraDyn, false)
	assert.False(t, infra.Allowed(workerFlag), "a worker-flag class must never become infra-allowed")
	assert.False(t, worker.Allowed("runner-bursty"), "a refused pair must drop the worker dynamic set too")
	assert.True(t, worker.Allowed(workerFlag), "the refused pair must fall back to the static flags")
	assert.True(t, infra.Allowed(infraFlag), "the refused pair must fall back to the static flags")

	_, err = gwValidator.ValidateCreate(ctx, v2GatewayWithScheduling(ns, "collide-rejected", workerFlag))
	require.Error(t, err, "the colliding class must be rejected at admission, not merely absent from a set")

	// Repair, then delete: enforcement follows the live object back up and fails safe
	// to the flags when it disappears.
	repaired := &v2beta1.PriorityClassAllowlist{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pcaName}, repaired))
	repaired.Spec.AllowedInfraPriorityClasses = []string{infraDyn}
	require.NoError(t, k8sClient.Update(ctx, repaired))
	waitForAllowed(t, infra, infraDyn, true)

	require.NoError(t, k8sClient.Delete(ctx, repaired))
	waitForAllowed(t, infra, infraDyn, false)
	assert.True(t, infra.Allowed(infraFlag), "the static infra flag must remain after the object is deleted")
}
