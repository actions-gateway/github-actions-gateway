//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGMC_RunnerGroupsDegraded_Rollup verifies Q158: a child RunnerGroup's
// impairing condition rolls up to a RunnerGroupsDegraded condition on the owning
// ActionsGateway (the operator's single pane), and the rollup clears when the
// child recovers. The GMC's RunnerGroup watch must drive the parent reconcile, so
// the assertions rely only on eventual consistency (no manual ActionsGateway poke).
func TestGMC_RunnerGroupsDegraded_Rollup(t *testing.T) {
	const nsName = "team-rg-rollup"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("rollup-gateway", nsName, "github-app")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// The GMC creates the owned RunnerGroup from the spec. Capture its (hashed) name.
	var rgKey types.NamespacedName
	g.Eventually(func() bool {
		var rgs agcv1alpha1.RunnerGroupList
		if err := k8sClient.List(ctx, &rgs, client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/owner-name": "rollup-gateway", "actions-gateway/owner-ns": nsName}); err != nil {
			return false
		}
		if len(rgs.Items) != 1 {
			return false
		}
		rgKey = types.NamespacedName{Namespace: nsName, Name: rgs.Items[0].Name}
		return true
	}, 15*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(), "GMC must create the owned RunnerGroup")

	// Initially the rollup is healthy (False) — the child carries no impairing condition.
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "rollup-gateway"}, &fetched); err != nil {
			return false
		}
		c := meta.FindStatusCondition(fetched.Status.Conditions, gmcv1alpha1.ConditionRunnerGroupsDegraded)
		return c != nil && c.Status == metav1.ConditionFalse
	}, 15*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(), "rollup starts healthy")

	// Trip an impairing condition on the child RunnerGroup's status.
	require.Eventually(t, func() bool {
		var rg agcv1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, rgKey, &rg); err != nil {
			return false
		}
		meta.SetStatusCondition(&rg.Status.Conditions, metav1.Condition{
			Type: agcv1alpha1.ConditionCredentialUnavailable, Status: metav1.ConditionTrue,
			Reason: agcv1alpha1.ReasonTokenUnavailable, Message: "test-induced",
		})
		return k8sClient.Status().Update(ctx, &rg) == nil
	}, 5*time.Second, 25*time.Millisecond, "set CredentialUnavailable on the child RunnerGroup")

	// The watch must drive a parent reconcile that flips RunnerGroupsDegraded=True,
	// naming the impaired group and its tripped condition.
	g.Eventually(func() *metav1.Condition {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "rollup-gateway"}, &fetched); err != nil {
			return nil
		}
		return meta.FindStatusCondition(fetched.Status.Conditions, gmcv1alpha1.ConditionRunnerGroupsDegraded)
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv1alpha1.ReasonRunnerGroupsImpaired),
		gomega.HaveField("Message", gomega.ContainSubstring(rgKey.Name)),
		gomega.HaveField("Message", gomega.ContainSubstring(agcv1alpha1.ConditionCredentialUnavailable)),
	), "a child's impairing condition must roll up to the ActionsGateway")

	// Clear the child condition; the rollup must recover to False.
	require.Eventually(t, func() bool {
		var rg agcv1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, rgKey, &rg); err != nil {
			return false
		}
		meta.SetStatusCondition(&rg.Status.Conditions, metav1.Condition{
			Type: agcv1alpha1.ConditionCredentialUnavailable, Status: metav1.ConditionFalse,
			Reason: agcv1alpha1.ReasonCredentialAvailable, Message: "recovered",
		})
		return k8sClient.Status().Update(ctx, &rg) == nil
	}, 5*time.Second, 25*time.Millisecond, "clear CredentialUnavailable on the child RunnerGroup")

	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "rollup-gateway"}, &fetched); err != nil {
			return false
		}
		c := meta.FindStatusCondition(fetched.Status.Conditions, gmcv1alpha1.ConditionRunnerGroupsDegraded)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == gmcv1alpha1.ReasonAllRunnerGroupsHealthy
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(), "rollup must clear when the child recovers")
}
