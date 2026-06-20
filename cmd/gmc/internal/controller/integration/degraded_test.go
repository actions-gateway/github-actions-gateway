//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestGMC_Degraded_OnProvisioningError verifies Q156: when a reconcile hits a
// provisioning error it returns before updateStatus, so without an explicit write
// the object's conditions would go stale. The reconciler must instead surface a
// Degraded=True condition naming the failing step before the early return — and
// clear it once the obstruction is removed and the reconcile succeeds.
//
// The provisioning error is forced realistically: a proxy Deployment with an
// immutable .spec.selector that differs from the reconciler's desired selector is
// pre-created, so the proxy-Deployment apply (a CreateOrPatch) is rejected by the
// apiserver — exactly the class of mid-reconcile failure that previously left
// conditions stale.
func TestGMC_Degraded_OnProvisioningError(t *testing.T) {
	const nsName = "team-degraded"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	// Pre-create a conflicting proxy Deployment BEFORE the reconciler starts, so the
	// first reconcile's proxy-Deployment apply patches an immutable selector and is
	// rejected. The selector intentionally differs from buildProxyDeployment's.
	conflicting := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyName, Namespace: nsName},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"q156": "conflict"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"q156": "conflict"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, conflicting))

	ag := newActionsGateway("degraded-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Degraded=True must appear, naming the failing step in its message.
	g.Eventually(func() *metav1.Condition {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "degraded-gateway"}, &fetched); err != nil {
			return nil
		}
		return meta.FindStatusCondition(fetched.Status.Conditions, gmcv1alpha1.ConditionDegraded)
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv1alpha1.ReasonProvisioningFailed),
		gomega.HaveField("Message", gomega.ContainSubstring("proxy Deployment")),
	), "a provisioning error must surface a Degraded condition naming the failing step")

	// Remove the obstruction: delete the conflicting Deployment so the reconciler
	// can create the proxy Deployment cleanly on the next reconcile.
	require.NoError(t, k8sClient.Delete(ctx, conflicting))

	// Once provisioning succeeds, updateStatus must clear Degraded (=False).
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "degraded-gateway"}, &fetched); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(fetched.Status.Conditions, gmcv1alpha1.ConditionDegraded)
		return cond != nil && cond.Status == metav1.ConditionFalse &&
			cond.Reason == gmcv1alpha1.ReasonReconcileSucceeded
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(),
		"Degraded must clear once the provisioning obstruction is removed")
}
