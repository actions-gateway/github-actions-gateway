//go:build integration

package integration_test

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

// TestCRD_ActionsGateway_MissingGitHubURL_Rejected verifies the apiserver rejects
// an ActionsGateway with no gitHubURL — the field is required (MinLength=1), so a
// gateway with nothing to register against cannot be created. This is enforced by
// the CRD OpenAPI schema, observable only at the envtest tier (a fake client does
// not validate required fields).
func TestCRD_ActionsGateway_MissingGitHubURL_Rejected(t *testing.T) {
	const nsName = "team-crd-nourl"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("nourl-ag", nsName, "github-app")
	ag.Spec.GitHubURL = ""
	err := k8sClient.Create(ctx, ag)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), ag)) })
	require.Error(t, err, "ActionsGateway without gitHubURL must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
}

// TestCRD_ActionsGateway_NonHTTPSGitHubURL_Rejected verifies the CRD Pattern guard
// (^https://) rejects a non-https URL at the apiserver even before the webhook runs.
func TestCRD_ActionsGateway_NonHTTPSGitHubURL_Rejected(t *testing.T) {
	const nsName = "team-crd-httpurl"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("httpurl-ag", nsName, "github-app")
	ag.Spec.GitHubURL = "http://github.com/example-org"
	err := k8sClient.Create(ctx, ag)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), ag)) })
	require.Error(t, err, "ActionsGateway with a non-https gitHubURL must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
}

// TestCRD_ActionsGateway_WebhookRejectsKubeSystem calls the webhook validator directly.
// In envtest the webhook server is not wired up with TLS, so we call the validator
// function directly — this tests the validation logic without the HTTP transport.
func TestCRD_ActionsGateway_WebhookRejectsKubeSystem(t *testing.T) {
	// Pass "gmc-system" as the custom install namespace too — exercises both
	// the default and runtime-derived reservation paths in one loop.
	validator := webhookv1alpha1.NewActionsGatewayCustomValidator("gmc-system")

	for _, ns := range []string{"kube-system", "kube-public", "gmc-system"} {
		ag := &gmcv1alpha1.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ag", Namespace: ns},
			Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"}},
		}
		_, err := validator.ValidateCreate(context.Background(), ag)
		require.Errorf(t, err, "namespace %q should be rejected by the webhook validator", ns)
		assert.Contains(t, err.Error(), "reserved", "error message should mention 'reserved'")
	}
}

// TestCRD_ActionsGateway_CrossNamespaceSecretRef_Rejected verifies that the webhook
// validator rejects a non-empty gitHubAppRef.namespace. The field has no effect
// (secretKeyRef ignores the namespace), but it looks cross-namespace to users —
// a confused-deputy footgun. Validated by the webhook rather than a CEL
// XValidation rule because k8s ≤ 1.30 CEL cannot use has() on optional
// non-pointer string fields; the webhook check is version-agnostic.
func TestCRD_ActionsGateway_CrossNamespaceSecretRef_Rejected(t *testing.T) {
	validator := webhookv1alpha1.NewActionsGatewayCustomValidator("")
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-ns-ag", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{
				Name:      "github-app",
				Namespace: "other-namespace",
			},
		},
	}
	_, err := validator.ValidateCreate(context.Background(), ag)
	require.Error(t, err, "ActionsGateway with gitHubAppRef.namespace set must be rejected by the webhook")
	assert.Contains(t, err.Error(), "gitHubAppRef.namespace is not supported")
}

// TestCRD_ProxyConfig_MaxReplicas_TooHigh_Rejected verifies that proxy.maxReplicas > 100
// is rejected by the CRD OpenAPI bounds (sanity ceiling; a per-cluster policy cap
// requires a future GMC flag).
func TestCRD_ProxyConfig_MaxReplicas_TooHigh_Rejected(t *testing.T) {
	const nsName = "team-cel-maxreplicas"
	createNamespace(t, nsName)

	ag := newActionsGateway("maxreplicas-ag", nsName, "github-app")
	maxReplicas := int32(101)
	ag.Spec.Proxy.MaxReplicas = &maxReplicas

	err := k8sClient.Create(ctx, ag)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), ag)) })

	require.Error(t, err, "ActionsGateway with proxy.maxReplicas > 100 must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
}

// TestCRD_ProxyConfig_MinExceedsMax_Rejected verifies that the CEL XValidation rule
// on ProxyConfig rejects specs where minReplicas > maxReplicas.
func TestCRD_ProxyConfig_MinExceedsMax_Rejected(t *testing.T) {
	const nsName = "team-cel-proxyorder"
	createNamespace(t, nsName)

	ag := newActionsGateway("proxyorder-ag", nsName, "github-app")
	minReplicas := int32(5)
	maxReplicas := int32(2)
	ag.Spec.Proxy.MinReplicas = &minReplicas
	ag.Spec.Proxy.MaxReplicas = &maxReplicas

	err := k8sClient.Create(ctx, ag)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), ag)) })

	require.Error(t, err, "ActionsGateway with proxy.minReplicas > proxy.maxReplicas must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
	assert.Contains(t, err.Error(), "minReplicas must not exceed maxReplicas",
		"error message should reference the ordering constraint")
}

// TestCRD_Tracing_InvalidSampler_Rejected verifies that spec.tracing.sampler is
// constrained to the OpenTelemetry built-in sampler enum by the CRD schema. The
// enum is enforced by the real apiserver, so this can only be proven at the
// envtest tier (a fake client does not validate OpenAPI bounds). A conforming
// value is accepted in the same test to guard against an over-tight enum.
func TestCRD_Tracing_InvalidSampler_Rejected(t *testing.T) {
	const nsName = "team-tracing-sampler"
	createNamespace(t, nsName)

	bad := newActionsGateway("bad-sampler-ag", nsName, "github-app")
	bad.Spec.Tracing = gmcv1alpha1.TracingConfig{
		Endpoint: "https://otel-collector.observability:4317",
		Sampler:  "ratio", // not a built-in OTEL sampler name
	}
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), bad)) })
	require.Error(t, err, "ActionsGateway with an unrecognized tracing.sampler must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)

	good := newActionsGateway("good-sampler-ag", nsName, "github-app")
	good.Spec.Tracing = gmcv1alpha1.TracingConfig{
		Endpoint:   "https://otel-collector.observability:4317",
		Sampler:    "parentbased_traceidratio",
		SamplerArg: "0.1",
	}
	require.NoError(t, k8sClient.Create(ctx, good),
		"ActionsGateway with a built-in tracing.sampler must be accepted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), good) })
}

// TestCRD_RunnerGroup_RunnerLabels_Validation verifies the D6 rule: an empty
// runnerLabels list (which would silently match every workflow) is rejected by
// MinItems, and a label containing whitespace is rejected by the item Pattern.
func TestCRD_RunnerGroup_RunnerLabels_Validation(t *testing.T) {
	const nsName = "team-cel-runnerlabels"
	createNamespace(t, nsName)

	minimalPodTemplate := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}

	emptyLabels := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-labels", Namespace: nsName},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 1,
			RunnerLabels: []string{},
			PodTemplate:  minimalPodTemplate,
		},
	}
	err := k8sClient.Create(ctx, emptyLabels)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), emptyLabels)) })
	require.Error(t, err, "RunnerGroup with empty runnerLabels must be rejected (MinItems=1)")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)

	whitespaceLabel := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "whitespace-label", Namespace: nsName},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 1,
			RunnerLabels: []string{"self hosted"},
			PodTemplate:  minimalPodTemplate,
		},
	}
	err = k8sClient.Create(ctx, whitespaceLabel)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), whitespaceLabel)) })
	require.Error(t, err, "RunnerGroup with a whitespace-containing label must be rejected by the item Pattern")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
}

func TestCRD_RunnerGroup_CELValidation_MaxWorkersConflict(t *testing.T) {
	const nsName = "team-cel-maxworkers"
	createNamespace(t, nsName)

	minimalPodTemplate := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}

	maxWorkers := int32(10)
	// Last tier threshold is 5 but maxWorkers is 10 — they must be equal.
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-maxworkers", Namespace: nsName},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 5,
			RunnerLabels: []string{"self-hosted"},
			MaxWorkers:   &maxWorkers,
			PodTemplate:  minimalPodTemplate,
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
			PodTemplate:  minimalPodTemplate,
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
