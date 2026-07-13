//go:build integration

package integration_test

import (
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests exercise the Q284 spec.scheduling.priorityClassName gate end-to-end
// against the real apiserver, for BOTH infra kinds. The suite wires the EgressProxy and
// the new v2 ActionsGateway webhooks to infraTestAllowlist (the single class
// "gag-infra-critical"), disjoint from the worker allowlist. A name on that list is
// admitted; any other named class is rejected; an empty/unset name always passes (so the
// other PodScheduling fields, e.g. topologySpreadConstraints, are usable without a class).

func schedulingZonalSpread() corev1.TopologySpreadConstraint {
	return corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "proxy"}},
	}
}

func TestV2_EgressProxy_Scheduling_AllowsAllowlistedPriorityClass(t *testing.T) {
	const ns = "v2-ep-sched-allow"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "allowed", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			Scheduling: &gmcv2alpha1.PodScheduling{
				PriorityClassName:         "gag-infra-critical",
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{schedulingZonalSpread()},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "an allowlisted infra PriorityClass must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
}

func TestV2_EgressProxy_Scheduling_RejectsOffAllowlistPriorityClass(t *testing.T) {
	const ns = "v2-ep-sched-deny"
	createNamespace(t, ns)

	// system-cluster-critical is nameable from any namespace (not kube-system-scoped) —
	// exactly the cluster-wide preemption escape the infra gate exists to close.
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			Scheduling: &gmcv2alpha1.PodScheduling{PriorityClassName: "system-cluster-critical"},
		},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err, "an off-allowlist infra PriorityClass must be rejected")
	assert.Contains(t, err.Error(), "infra allowlist")
}

func TestV2_EgressProxy_Scheduling_EmptyPriorityClassAlwaysAllowed(t *testing.T) {
	const ns = "v2-ep-sched-empty"
	createNamespace(t, ns)

	// No priorityClassName, only a topologySpreadConstraint: the ungated fields are
	// usable on their own, and an empty name never trips the gate.
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			Scheduling: &gmcv2alpha1.PodScheduling{
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{schedulingZonalSpread()},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "an unset priorityClassName must always be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
}

func TestV2_ActionsGateway_Scheduling_AllowsAllowlistedPriorityClass(t *testing.T) {
	const ns = "v2-ag-sched-allow"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	ag := newV2GatewayWired("allowed", ns, "github-app", "")
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
		PriorityClassName:         "gag-infra-critical",
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{schedulingZonalSpread()},
	}
	require.NoError(t, k8sClient.Create(ctx, ag), "an allowlisted infra PriorityClass must be admitted on a v2 ActionsGateway")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })
}

func TestV2_ActionsGateway_Scheduling_RejectsOffAllowlistPriorityClass(t *testing.T) {
	const ns = "v2-ag-sched-deny"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	ag := newV2GatewayWired("offlist", ns, "github-app", "")
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{PriorityClassName: "system-cluster-critical"}
	err := k8sClient.Create(ctx, ag)
	require.Error(t, err, "an off-allowlist infra PriorityClass must be rejected on a v2 ActionsGateway")
	assert.Contains(t, err.Error(), "infra allowlist")
}
