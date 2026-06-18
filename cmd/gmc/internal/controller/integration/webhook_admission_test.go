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
	// Each CR lives in its own namespace: the one-ActionsGateway-per-namespace
	// singleton guard forbids more than one CR per namespace, so the three
	// transition scenarios below cannot share a namespace.
	const downgradeNS = "team-webhook-profile-downgrade"
	const upgradeNS = "team-webhook-profile-upgrade"
	const annotatedNS = "team-webhook-profile-annotated"
	createNamespace(t, downgradeNS)
	createNamespace(t, upgradeNS)
	createNamespace(t, annotatedNS)

	// Reject: restricted -> baseline without the annotation.
	downgrade := newActionsGateway("downgrade-ag", downgradeNS, "github-app")
	downgrade.Spec.SecurityProfile = "restricted"
	require.NoError(t, k8sClient.Create(ctx, downgrade),
		"creating a restricted-profile ActionsGateway must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), downgrade) })

	err := updateActionsGateway(t, downgradeNS, "downgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		ag.Spec.SecurityProfile = "baseline"
	})
	require.Error(t, err, "lowering securityProfile without the annotation must be rejected by the webhook")
	assert.Contains(t, err.Error(), "downgrade",
		"rejection must come from the GMC validating webhook")

	// Admit: baseline -> restricted (an upgrade) on a fresh CR.
	upgrade := newActionsGateway("upgrade-ag", upgradeNS, "github-app")
	upgrade.Spec.SecurityProfile = "baseline"
	require.NoError(t, k8sClient.Create(ctx, upgrade))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), upgrade) })
	require.NoError(t, updateActionsGateway(t, upgradeNS, "upgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		ag.Spec.SecurityProfile = "restricted"
	}), "raising securityProfile (an upgrade) must be admitted")

	// Admit: restricted -> baseline once the allow-downgrade annotation is set.
	annotated := newActionsGateway("annotated-downgrade-ag", annotatedNS, "github-app")
	annotated.Spec.SecurityProfile = "restricted"
	require.NoError(t, k8sClient.Create(ctx, annotated))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), annotated) })
	require.NoError(t, updateActionsGateway(t, annotatedNS, "annotated-downgrade-ag", func(ag *gmcv1alpha1.ActionsGateway) {
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] = "true"
		ag.Spec.SecurityProfile = "baseline"
	}), "a deliberate downgrade carrying the allow-downgrade annotation must be admitted")
}

// TestWebhookAdmission_Singleton verifies the apiserver admits the first
// ActionsGateway in a namespace, rejects a second one in the SAME namespace, and
// still admits one in a DIFFERENT namespace (the guard is per-namespace, not
// global).
func TestWebhookAdmission_Singleton(t *testing.T) {
	const nsName = "team-webhook-singleton"
	const otherNS = "team-webhook-singleton-other"
	createNamespace(t, nsName)
	createNamespace(t, otherNS)

	first := newActionsGateway("first-ag", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, first),
		"the first ActionsGateway in a namespace must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), first)) })

	second := newActionsGateway("second-ag", nsName, "github-app")
	err := k8sClient.Create(ctx, second)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), second)) })
	require.Error(t, err, "a second ActionsGateway in the same namespace must be rejected by the webhook")
	assert.Contains(t, err.Error(), "only one ActionsGateway per namespace is supported",
		"rejection must come from the GMC validating webhook singleton guard")

	// A different namespace is independent — its first CR is admitted.
	otherFirst := newActionsGateway("other-first-ag", otherNS, "github-app")
	require.NoError(t, k8sClient.Create(ctx, otherFirst),
		"the singleton guard is per-namespace; a different namespace's first CR must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), otherFirst)) })
}

// TestWebhookAdmission_PrivilegedProfileAllowsPrivilegedContainer verifies that
// once a tenant explicitly opts into securityProfile: privileged, the apiserver
// admits the documented Kata/DinD privileged worker pattern that is otherwise
// rejected under the (default) baseline profile. The namespace carries the
// platform-applied privileged-profile label (Q133), without which privileged would
// be ineligible regardless of the container's securityContext.
func TestWebhookAdmission_PrivilegedProfileAllowsPrivilegedContainer(t *testing.T) {
	const nsName = "team-webhook-privileged-profile"
	createNamespaceWithLabels(t, nsName, map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	})

	privileged := true
	rg := agcv1alpha1.RunnerGroupSpec{
		RunnerLabels: []string{"self-hosted"},
		MaxListeners: 1,
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "runner", Image: "runner:test", SecurityContext: &corev1.SecurityContext{Privileged: &privileged}},
				},
			},
		},
	}

	ag := newActionsGateway("privileged-profile-ag", nsName, "github-app")
	ag.Spec.SecurityProfile = "privileged"
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{rg}
	require.NoError(t, k8sClient.Create(ctx, ag),
		"a privileged container under securityProfile: privileged must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), ag)) })
}

// TestWebhookAdmission_PrivilegedProfileEligibility verifies the platform-gated
// eligibility for securityProfile: privileged through the real apiserver (Q133).
// privileged is rejected at CREATE and at UPDATE in a namespace that lacks the
// platform-applied privileged-profile label, and admitted once the label is
// present — closing the create-time self-escalation gap (a tenant could
// otherwise self-select the cluster's least-restrictive PSA posture). The label
// is a platform decision, never tenant-settable.
func TestWebhookAdmission_PrivilegedProfileEligibility(t *testing.T) {
	const unlabeledNS = "team-webhook-privileged-eligibility-unlabeled"
	const labeledNS = "team-webhook-privileged-eligibility-labeled"
	createNamespace(t, unlabeledNS) // no privileged-profile label
	createNamespaceWithLabels(t, labeledNS, map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	})

	// CREATE rejected: privileged in an unlabeled namespace is a self-granted
	// escalation; fail closed.
	bad := newActionsGateway("priv-eligibility-create", unlabeledNS, "github-app")
	bad.Spec.SecurityProfile = "privileged"
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), bad)) })
	require.Error(t, err, "creating a privileged-profile ActionsGateway in an unlabeled namespace must be rejected")
	assert.Contains(t, err.Error(), "not eligible",
		"rejection must come from the GMC privileged-eligibility gate")
	assert.Contains(t, err.Error(), gmcv1alpha1.PrivilegedProfileLabel)

	// CREATE admitted: same request in a labeled namespace.
	good := newActionsGateway("priv-eligibility-ok", labeledNS, "github-app")
	good.Spec.SecurityProfile = "privileged"
	require.NoError(t, k8sClient.Create(ctx, good),
		"privileged is eligible once the platform labels the namespace")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), good)) })

	// UPDATE rejected: a baseline CR raised to privileged in an unlabeled
	// namespace is rejected even with the downgrade annotation (the platform
	// label is an independent, fail-closed requirement).
	upgradeTarget := newActionsGateway("priv-eligibility-update", unlabeledNS, "github-app")
	upgradeTarget.Spec.SecurityProfile = "baseline"
	require.NoError(t, k8sClient.Create(ctx, upgradeTarget))
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), upgradeTarget)) })
	err = updateActionsGateway(t, unlabeledNS, "priv-eligibility-update", func(ag *gmcv1alpha1.ActionsGateway) {
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] = "true"
		ag.Spec.SecurityProfile = "privileged"
	})
	require.Error(t, err, "raising securityProfile to privileged in an unlabeled namespace must be rejected on update")
	assert.Contains(t, err.Error(), gmcv1alpha1.PrivilegedProfileLabel)
}

// TestWebhookAdmission_NoProxyCIDRs verifies the apiserver rejects a
// non-CIDR proxy.noProxyCIDRs entry (which would silently route matching traffic
// around the per-tenant egress proxy) and admits well-formed CIDRs.
func TestWebhookAdmission_NoProxyCIDRs(t *testing.T) {
	const nsName = "team-webhook-noproxy"
	createNamespace(t, nsName)

	bad := newActionsGateway("noproxy-github-ag", nsName, "github-app")
	bad.Spec.Proxy.NoProxyCIDRs = []string{"github.com"}
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), bad)) })
	require.Error(t, err, "a GitHub host in noProxyCIDRs must be rejected by the webhook through the apiserver")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy",
		"rejection must come from the GMC validating webhook")

	// CIDRs and non-GitHub domain suffixes (svc.cluster.local — the in-cluster
	// pattern the e2e relies on) must be admitted.
	good := newActionsGateway("noproxy-ok-ag", nsName, "github-app")
	good.Spec.Proxy.NoProxyCIDRs = []string{"10.0.0.0/8", "203.0.113.5/32", "svc.cluster.local"}
	require.NoError(t, k8sClient.Create(ctx, good),
		"CIDRs and non-GitHub domain suffixes in noProxyCIDRs must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), good)) })
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
