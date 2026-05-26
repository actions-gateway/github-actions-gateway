//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
)

// TestGMC_CredRotation_PodTemplateAnnotation verifies that the AGC pod template
// carries an annotation recording the referenced Secret name, and that the
// annotation updates when gitHubAppRef.Name changes (making the rotation visible
// in kubectl rollout history).
func TestGMC_CredRotation_PodTemplateAnnotation(t *testing.T) {
	const nsName = "team-cred-annotation"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "secret-v1")
	createGitHubAppSecret(t, nsName, "secret-v2")

	ag := newActionsGateway("ann-gateway", nsName, "secret-v1")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Initial reconcile: annotation must reference secret-v1.
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep); err != nil {
			return ""
		}
		return dep.Spec.Template.Annotations["actions-gateway/github-app-secret"]
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Equal("secret-v1"))

	// Rotate: update gitHubAppRef.Name to secret-v2.
	require.Eventually(t, func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "ann-gateway"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.GitHubAppRef.Name = "secret-v2"
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update gitHubAppRef to secret-v2")

	// After rotation the annotation must reflect secret-v2.
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep); err != nil {
			return ""
		}
		return dep.Spec.Template.Annotations["actions-gateway/github-app-secret"]
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Equal("secret-v2"))
}

// TestGMC_CredRotation_CredentialUnavailableOnSecretDelete verifies that deleting
// the referenced Secret causes a CredentialUnavailable=True condition on the
// ActionsGateway within one reconcile cycle.
func TestGMC_CredRotation_CredentialUnavailableOnSecretDelete(t *testing.T) {
	const nsName = "team-cred-unavailable"
	createNamespace(t, nsName)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "deletable-secret", Namespace: nsName},
		Data: map[string][]byte{
			"appId": []byte("1"), "privateKey": []byte("k"), "installationId": []byte("2"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))

	ag := newActionsGateway("unavail-gateway", nsName, "deletable-secret")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for initial healthy reconcile.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Delete the Secret.
	require.NoError(t, k8sClient.Delete(ctx, secret))

	// CredentialUnavailable=True must appear on the ActionsGateway.
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "unavail-gateway"}, &fetched); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(fetched.Status.Conditions, "CredentialUnavailable")
		return cond != nil && cond.Status == "True"
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"CredentialUnavailable condition must be set after Secret deletion")
}

// TestGMC_CredRotation_InPlaceUpdateNoRollout verifies that updating the Secret
// contents without changing gitHubAppRef.Name does NOT change the AGC Deployment
// pod template (no spurious rollout).
func TestGMC_CredRotation_InPlaceUpdateNoRollout(t *testing.T) {
	const nsName = "team-cred-inplace"
	createNamespace(t, nsName)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-secret", Namespace: nsName},
		Data: map[string][]byte{
			"appId": []byte("1"), "privateKey": []byte("original"), "installationId": []byte("2"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

	ag := newActionsGateway("inplace-gateway", nsName, "stable-secret")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for the AGC Deployment to be created and note its resource version.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var depBefore appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &depBefore))
	templateRVBefore := depBefore.Spec.Template.Annotations["actions-gateway/github-app-secret"]

	// Update the Secret in place (same name, new content).
	require.Eventually(t, func() bool {
		var s corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "stable-secret"}, &s); err != nil {
			return false
		}
		s.Data["privateKey"] = []byte("rotated-in-place")
		return k8sClient.Update(ctx, &s) == nil
	}, 5*time.Second, 25*time.Millisecond, "update Secret contents in-place")

	// Give the controller time to react to any spurious reconcile.
	time.Sleep(200 * time.Millisecond)

	// The annotation must still reference "stable-secret" (unchanged).
	var depAfter appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &depAfter))
	require.Equal(t, templateRVBefore, depAfter.Spec.Template.Annotations["actions-gateway/github-app-secret"],
		"in-place Secret update must not change the pod template annotation")
}
