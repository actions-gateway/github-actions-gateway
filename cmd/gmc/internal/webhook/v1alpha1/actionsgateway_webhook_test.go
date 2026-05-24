package v1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
)

func newAG(namespace string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
		},
	}
}

func TestWebhook_RejectsKubeSystem(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("kube-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-system")
}

func TestWebhook_RejectsKubePublic(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("kube-public"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-public")
}

func TestWebhook_RejectsGMCSystem(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("actions-gateway-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions-gateway-system")
}

func TestWebhook_AllowsTenantNamespace(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}

func TestWebhook_UpdateNoOp(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateUpdate(context.Background(), newAG("kube-system"), newAG("kube-system"))
	require.NoError(t, err)
}

func TestWebhook_DeleteNoOp(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}
