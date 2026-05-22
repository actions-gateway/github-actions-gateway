//go:build integration

package integration_test

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	webhookv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCRD_ValidActionsGateway_Accepted(t *testing.T) {
	const nsName = "team-crd-valid"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("valid-ag", nsName, "github-app")
	err := k8sClient.Create(ctx, ag)
	require.NoError(t, err, "a valid ActionsGateway CR must be accepted by the API server")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	var fetched gmcv1alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "valid-ag"}, &fetched))
	assert.Equal(t, "github-app", fetched.Spec.GitHubAppRef.Name)
}

// TestCRD_ActionsGateway_WebhookRejectsKubeSystem calls the webhook validator directly.
// In envtest the webhook server is not wired up with TLS, so we call the validator
// function directly — this tests the validation logic without the HTTP transport.
func TestCRD_ActionsGateway_WebhookRejectsKubeSystem(t *testing.T) {
	validator := &webhookv1alpha1.ActionsGatewayCustomValidator{}

	for _, ns := range []string{"kube-system", "kube-public", "actions-gateway-system"} {
		ag := &gmcv1alpha1.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ag", Namespace: ns},
			Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"}},
		}
		_, err := validator.ValidateCreate(context.Background(), ag)
		require.Errorf(t, err, "namespace %q should be rejected by the webhook validator", ns)
		assert.Contains(t, err.Error(), "reserved", "error message should mention 'reserved'")
	}
}


func TestCRD_RunnerGroup_CELValidation_MaxWorkersConflict(t *testing.T) {
	const nsName = "team-cel-maxworkers"
	createNamespace(t, nsName)

	maxWorkers := int32(10)
	// Last tier threshold is 5 but maxWorkers is 10 — they must be equal.
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-maxworkers", Namespace: nsName},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 5,
			RunnerLabels: []string{"self-hosted"},
			MaxWorkers:   &maxWorkers,
			PriorityTiers: []agcv1alpha1.PriorityTier{
				{PriorityClassName: "standard", Threshold: 5},
			},
		},
	}
	err := k8sClient.Create(ctx, rg)
	require.Error(t, err, "RunnerGroup where maxWorkers != lastTier.Threshold must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
	assert.Contains(t, err.Error(), "threshold",
		"error message should mention threshold")

	t.Cleanup(func() {
		// Only attempt to delete if creation somehow succeeded.
		_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), rg))
	})

	// Verify a conforming spec (maxWorkers == lastTier.Threshold) is accepted.
	maxWorkers2 := int32(5)
	validRG := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-maxworkers", Namespace: nsName},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 5,
			RunnerLabels: []string{"self-hosted"},
			MaxWorkers:   &maxWorkers2,
			PriorityTiers: []agcv1alpha1.PriorityTier{
				{PriorityClassName: "standard", Threshold: 5},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, validRG),
		"RunnerGroup where maxWorkers == lastTier.Threshold must be accepted")
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), validRG)
	})

}
