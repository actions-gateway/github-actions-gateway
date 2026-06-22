//go:build integration

package integration_test

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// representativeMigrationInput builds the acceptance scenario from Q165: a tenant
// with multiple groups (a shared template + a distinct one), a proxy, and a
// non-default securityProfile.
func representativeMigrationInput(ns string) migrate.Input {
	min := int32(2)
	max := int32(8)
	gw := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef:    gmcv1alpha1.SecretReference{Name: "github-app"},
			GitHubURL:       "https://github.com/example-org",
			SecurityProfile: "restricted",
			Proxy:           gmcv1alpha1.ProxyConfig{MinReplicas: &min, MaxReplicas: &max},
		},
	}
	podTemplate := func(img string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: img}}},
		}
	}
	rg := func(name, img string, labels []string) agcv1alpha1.RunnerGroup {
		return agcv1alpha1.RunnerGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: agcv1alpha1.RunnerGroupSpec{
				RunnerLabels: labels,
				PodTemplate:  podTemplate(img),
				WorkerImage:  "worker:" + img,
			},
		}
	}
	maxWorkers := int32(20)
	gpu := rg("team-gpu", "gpu", []string{"gpu"})
	gpu.Spec.MaxWorkers = &maxWorkers
	gpu.Spec.PriorityTiers = []agcv1alpha1.PriorityTier{{PriorityClassName: "high", Threshold: 20}}

	return migrate.Input{
		Namespace:       ns,
		NamespaceLabels: map[string]string{gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue},
		Gateway:         gw,
		RunnerGroups: []agcv1alpha1.RunnerGroup{
			rg("team-linux", "linux", []string{"linux"}),
			rg("team-linux2", "linux", []string{"linux2"}), // identical template → reuse
			gpu,
		},
	}
}

// TestGMC_Migration_ApplyEmittedObjectsPassV2CEL is the M5 `--apply`-path acceptance
// check (§H.17 invariant: "v1 data must be admissible under v2 CEL"). It fans a
// representative v1 tenant out and creates every emitted object against the real
// apiserver — so the v2 CRD CEL (name ≤52, maxWorkers == priorityTiers[last].threshold,
// reserved-pod-field rules) and the RunnerTemplate reserved-field webhook all run. A
// pure-Go transform check cannot catch a CEL/defaulting gap; this envtest can.
func TestGMC_Migration_ApplyEmittedObjectsPassV2CEL(t *testing.T) {
	const ns = "migrate-apply"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
	})

	res, err := migrate.FanOut(representativeMigrationInput(ns))
	require.NoError(t, err)
	require.Empty(t, res.Warnings, "the representative tenant migrates without warnings")

	// Reuse invariant holds in the emitted set: 3 groups → 2 templates, 3 sets.
	assert.Len(t, res.Templates, 2)
	assert.Len(t, res.Sets, 3)

	// Apply children before referrers, mirroring the CLI. Every Create exercises the
	// real admission path; a CEL/webhook rejection fails the test.
	require.NoError(t, k8sClient.Create(ctx, res.Proxy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), res.Proxy) })
	for _, tmpl := range res.Templates {
		require.NoErrorf(t, k8sClient.Create(ctx, tmpl), "RunnerTemplate %q must pass admission", tmpl.Name)
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	}
	require.NoError(t, k8sClient.Create(ctx, res.Gateway))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), res.Gateway) })
	for _, set := range res.Sets {
		require.NoErrorf(t, k8sClient.Create(ctx, set), "RunnerSet %q must pass admission", set.Name)
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), set) })
	}

	// The emitted gateway wires defaultProxyRef (no silent direct egress) and every
	// set inherits it (proxyRef unset).
	got := &gmcv2alpha1.ActionsGateway{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: res.Gateway.Name}, got))
	require.NotNil(t, got.Spec.DefaultProxyRef)
	assert.Equal(t, res.Proxy.Name, got.Spec.DefaultProxyRef.Name)
	for _, set := range res.Sets {
		gotSet := &gmcv2alpha1.RunnerSet{}
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: set.Name}, gotSet))
		assert.Nil(t, gotSet.Spec.ProxyRef, "set %q inherits defaultProxyRef (proxied, not direct)", set.Name)
		assert.Equal(t, int32(1), gotSet.Spec.MaxListeners, "v1 maxListeners ceiling preserved")
	}
}

// TestGMC_Migration_NamespacePatchAcceptedByVAP applies the relocated
// securityProfile label produced by the migration and verifies the shipped
// namespace-security-profile-guard VAP admits it on a managed tenant namespace — the
// `--apply` namespace-patch path end to end.
func TestGMC_Migration_NamespacePatchAcceptedByVAP(t *testing.T) {
	installSecurityProfileGuard(t)

	const ns = "migrate-nspatch"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
	})

	res, err := migrate.FanOut(representativeMigrationInput(ns))
	require.NoError(t, err)
	require.Equal(t, gmcv2alpha1.SecurityProfileRestricted, res.NamespacePatch.Labels[gmcv2alpha1.SecurityProfileLabel])

	require.NoError(t, setNamespaceLabel(k8sClient, ns, func(n *corev1.Namespace) {
		for k, v := range res.NamespacePatch.Labels {
			n.Labels[k] = v
		}
	}), "the relocated security-profile label must be admitted by the VAP")

	got := &corev1.Namespace{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ns}, got))
	assert.Equal(t, gmcv2alpha1.SecurityProfileRestricted, got.Labels[gmcv2alpha1.SecurityProfileLabel])
}

// TestGMC_Migration_DualReadV1WebhookAcceptsV2Grant proves the dual-read window
// completion: after the migration relabels a namespace's privileged-profile grant
// onto the actions-gateway.com domain, a still-running v1 ActionsGateway requesting
// securityProfile=privileged is still admitted by the v1 webhook (end to end through
// the apiserver). Without the dual-read the relabel would strand the v1 gateway.
func TestGMC_Migration_DualReadV1WebhookAcceptsV2Grant(t *testing.T) {
	const ns = "migrate-dualread"
	// Only the v2-domain privileged grant is present (as the migration would leave it).
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
		gmcv2alpha1.PrivilegedProfileLabel:     gmcv2alpha1.PrivilegedProfileAllowed,
	})
	createGitHubAppSecret(t, ns, "github-app")

	ag := newActionsGateway("v1-priv", ns, "github-app")
	ag.Spec.SecurityProfile = "privileged"
	require.NoError(t, k8sClient.Create(ctx, ag),
		"the v1 webhook must accept privileged when only the v2-domain grant is present (dual-read)")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
}

// TestGMC_Migration_Coexistence proves a v1 ActionsGateway and the migrated v2
// ActionsGateway live side by side in one namespace (the no-behavior-change /
// rollback-by-staying-on-v1 contract): both admit, neither blocks the other.
func TestGMC_Migration_Coexistence(t *testing.T) {
	const ns = "migrate-coexist"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
	})
	createGitHubAppSecret(t, ns, "github-app")

	// v1 gateway keeps running.
	v1 := newActionsGateway("legacy", ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, v1))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), v1) })

	// v2 fan-out applied beside it.
	res, err := migrate.FanOut(representativeMigrationInput(ns))
	require.NoError(t, err)
	require.NoError(t, k8sClient.Create(ctx, res.Proxy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), res.Proxy) })
	require.NoError(t, k8sClient.Create(ctx, res.Gateway))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), res.Gateway) })

	// Both visible.
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "legacy"}, &gmcv1alpha1.ActionsGateway{}))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: res.Gateway.Name}, &gmcv2alpha1.ActionsGateway{}))
}
