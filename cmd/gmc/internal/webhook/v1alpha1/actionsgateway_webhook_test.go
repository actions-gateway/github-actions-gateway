package v1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
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
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), newAG("kube-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-system")
}

func TestWebhook_RejectsKubePublic(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), newAG("kube-public"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-public")
}

// TestWebhook_RejectsDefaultGMCNamespace covers the default install namespace.
// Even when the GMC has no POD_NAMESPACE env var (e.g. `make run` outside a
// pod, or a misconfigured Deployment that drops the downward-API mapping),
// `gmc-system` must still be reserved as a backstop.
func TestWebhook_RejectsDefaultGMCNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), newAG("gmc-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gmc-system")
}

// TestWebhook_RejectsCustomInstallNamespace covers a non-default install
// (e.g. an operator deployed to `actions-gateway-operator`). The downward
// API supplies the install namespace and the webhook must reject CRs in it.
func TestWebhook_RejectsCustomInstallNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("actions-gateway-operator")
	_, err := v.ValidateCreate(context.Background(), newAG("actions-gateway-operator"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions-gateway-operator")
}

func TestWebhook_AllowsTenantNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}

func TestWebhook_UpdateAllowsSafe(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), newAG("team-a"))
	require.NoError(t, err)
}

// ptr returns a pointer to v — helper for SecurityContext fields.
func ptr[T any](v T) *T { return &v }

func agWithPrivilegedContainer(privileged bool) *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "runner",
							Image: "runner:latest",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr(privileged),
							},
						},
					},
				},
			},
		},
	}
	return ag
}

func agWithPrivilegedInitContainer() *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:latest"}},
					InitContainers: []corev1.Container{
						{
							Name:  "init",
							Image: "busybox",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr(true),
							},
						},
					},
				},
			},
		},
	}
	return ag
}

func TestWebhook_RejectsPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedContainer(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

func TestWebhook_AllowsNonPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedContainer(false))
	require.NoError(t, err)
}

func TestWebhook_RejectsPrivilegedInitContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedInitContainer())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged init containers are not permitted")
}

func TestWebhook_UpdateRejectsPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), agWithPrivilegedContainer(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

func TestWebhook_DeleteNoOp(t *testing.T) {
	v := NewActionsGatewayCustomValidator("")
	_, err := v.ValidateDelete(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}
