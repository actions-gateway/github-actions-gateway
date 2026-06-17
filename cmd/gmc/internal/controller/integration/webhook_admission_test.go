//go:build integration

package integration_test

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The tests in this file drive the GMC validating webhook through the real
// apiserver (client.Create / client.Update) rather than calling the validator
// function directly. The webhook is registered with the envtest apiserver in
// TestMain (failurePolicy=Fail), so a rejected request proves the admission
// path — apiserver -> webhook -> CA -> validator -> denial -> error to client —
// works end to end, not just the validation logic in isolation. The
// direct-call coverage in crd_admission_test.go stays; this complements it at
// the admission-through-apiserver tier (Q75).

// TestWebhookAdmission_GitHubAppRef verifies the apiserver rejects an
// ActionsGateway whose gitHubAppRef.namespace is set (a cross-namespace
// confused-deputy footgun) and admits one that leaves it empty.
func TestWebhookAdmission_GitHubAppRef(t *testing.T) {
	const nsName = "team-webhook-appref"
	createNamespace(t, nsName)

	bad := newActionsGateway("appref-cross-ns", nsName, "github-app")
	bad.Spec.GitHubAppRef.Namespace = "other-namespace"
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), bad)) })
	require.Error(t, err, "ActionsGateway with gitHubAppRef.namespace set must be rejected by the webhook through the apiserver")
	assert.Contains(t, err.Error(), "gitHubAppRef.namespace is not supported",
		"rejection must come from the GMC validating webhook")

	good := newActionsGateway("appref-same-ns", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, good),
		"ActionsGateway with an empty gitHubAppRef.namespace must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), good) })
}

// TestWebhookAdmission_PrivilegedContainer verifies the apiserver rejects an
// ActionsGateway whose RunnerGroup pod template requests a privileged container
// and admits an otherwise-identical non-privileged one.
func TestWebhookAdmission_PrivilegedContainer(t *testing.T) {
	const nsName = "team-webhook-privileged"
	createNamespace(t, nsName)

	runnerGroup := func(privileged bool) agcv1alpha1.RunnerGroupSpec {
		sc := &corev1.SecurityContext{}
		if privileged {
			sc.Privileged = &privileged
		}
		return agcv1alpha1.RunnerGroupSpec{
			RunnerLabels: []string{"self-hosted"},
			MaxListeners: 1,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "runner", Image: "runner:test", SecurityContext: sc},
					},
				},
			},
		}
	}

	bad := newActionsGateway("privileged-ag", nsName, "github-app")
	bad.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{runnerGroup(true)}
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), bad)) })
	require.Error(t, err, "ActionsGateway with a privileged container must be rejected by the webhook through the apiserver")
	assert.Contains(t, err.Error(), "privileged containers are not permitted",
		"rejection must come from the GMC validating webhook")

	good := newActionsGateway("unprivileged-ag", nsName, "github-app")
	good.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{runnerGroup(false)}
	require.NoError(t, k8sClient.Create(ctx, good),
		"ActionsGateway with a non-privileged container must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), good) })
}

// TestWebhookAdmission_SecurityProfileDowngrade verifies the apiserver rejects
// an update that lowers spec.securityProfile (restricted -> baseline) without
// the allow-downgrade annotation, admits a non-downgrade update (baseline ->
// restricted), and admits the same downgrade once the annotation is present.
func TestWebhookAdmission_SecurityProfileDowngrade(t *testing.T) {
	const nsName = "team-webhook-profile"
	createNamespace(t, nsName)

	// Reject: restricted -> baseline without the annotation.
	downgrade := newActionsGateway("downgrade-ag", nsName, "github-app")
	downgrade.Spec.SecurityProfile = "restricted"
	require.NoError(t, k8sClient.Create(ctx, downgrade),
		"creating a restricted-profile ActionsGateway must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), downgrade) })

	err := updateActionsGateway(t, nsName, "downgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		ag.Spec.SecurityProfile = "baseline"
	})
	require.Error(t, err, "lowering securityProfile without the annotation must be rejected by the webhook")
	assert.Contains(t, err.Error(), "downgrade",
		"rejection must come from the GMC validating webhook")

	// Admit: baseline -> restricted (an upgrade) on a fresh CR.
	upgrade := newActionsGateway("upgrade-ag", nsName, "github-app")
	upgrade.Spec.SecurityProfile = "baseline"
	require.NoError(t, k8sClient.Create(ctx, upgrade))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), upgrade) })
	require.NoError(t, updateActionsGateway(t, nsName, "upgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		ag.Spec.SecurityProfile = "restricted"
	}), "raising securityProfile (an upgrade) must be admitted")

	// Admit: restricted -> baseline once the allow-downgrade annotation is set.
	annotated := newActionsGateway("annotated-downgrade-ag", nsName, "github-app")
	annotated.Spec.SecurityProfile = "restricted"
	require.NoError(t, k8sClient.Create(ctx, annotated))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), annotated) })
	require.NoError(t, updateActionsGateway(t, nsName, "annotated-downgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] = "true"
		ag.Spec.SecurityProfile = "baseline"
	}), "a deliberate downgrade carrying the allow-downgrade annotation must be admitted")
}

// updateActionsGateway fetches the latest version of the named ActionsGateway,
// applies mutate, and issues the Update, returning the admission outcome (nil =
// admitted, error = rejected). Fetching the live object (rather than reusing the
// create-time struct) avoids a spurious resourceVersion conflict that would mask
// the admission result under test.
func updateActionsGateway(t *testing.T, ns, name string, mutate func(*gmcv1alpha1.ActionsGateway)) error {
	t.Helper()
	var ag gmcv1alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ag))
	mutate(&ag)
	return k8sClient.Update(ctx, &ag)
}
