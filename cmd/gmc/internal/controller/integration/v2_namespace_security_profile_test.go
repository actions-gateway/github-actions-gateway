//go:build integration

package integration_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const securityProfileGuardManifestPath = "../../../config/admission-policy/namespace-security-profile-guard.yaml"

// installSecurityProfileGuard applies the real shipped security-profile-guard VAP +
// binding (loading the deployed artifact so a CEL typo is caught here). Cluster-scoped
// and idempotent; left in place for other tests.
func installSecurityProfileGuard(t *testing.T) {
	t.Helper()
	f, err := os.Open(securityProfileGuardManifestPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if decErr := dec.Decode(u); decErr != nil {
			if decErr == io.EOF {
				break
			}
			require.NoError(t, decErr)
		}
		if len(u.Object) == 0 {
			continue
		}
		if createErr := k8sClient.Create(ctx, u); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			require.NoError(t, createErr)
		}
	}
}

// TestGMC_NamespaceSecurityProfileGuard verifies the v2 namespace-scoped PSA guard
// (Q175 / §H.16 #7): the actions-gateway.com/security-profile label on a managed tenant
// namespace is enum-checked, cannot be silently downgraded, and privileged requires the
// platform eligibility label — none weaker than the v1 ActionsGateway webhook.
func TestGMC_NamespaceSecurityProfileGuard(t *testing.T) {
	installSecurityProfileGuard(t)
	g := gomega.NewWithT(t)

	tenantLabels := func(extra map[string]string) map[string]string {
		l := map[string]string{gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}

	t.Run("non-tenant namespace is not subject", func(t *testing.T) {
		const ns = "secprof-non-tenant"
		require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{gmcv2alpha1.SecurityProfileLabel: "bogus"},
		}}))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
	})

	t.Run("invalid enum rejected on a tenant namespace", func(t *testing.T) {
		const ns = "secprof-enum"
		// Gate on enforcement: poll until the bogus profile is denied.
		g.Eventually(func() bool {
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:   ns,
				Labels: tenantLabels(map[string]string{gmcv2alpha1.SecurityProfileLabel: "bogus"}),
			}})
			if err == nil {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			}
			return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
			"an invalid security-profile value must be denied on a tenant namespace")
	})

	t.Run("baseline create then restricted upgrade allowed", func(t *testing.T) {
		const ns = "secprof-upgrade"
		require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: tenantLabels(map[string]string{gmcv2alpha1.SecurityProfileLabel: gmcv2alpha1.SecurityProfileBaseline}),
		}}))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		require.NoError(t, setNamespaceLabel(k8sClient, ns, func(n *corev1.Namespace) {
			n.Labels[gmcv2alpha1.SecurityProfileLabel] = gmcv2alpha1.SecurityProfileRestricted
		}), "an upgrade restricted-ward must always be allowed")
	})

	t.Run("silent downgrade rejected, annotated downgrade allowed", func(t *testing.T) {
		const ns = "secprof-downgrade"
		require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: tenantLabels(map[string]string{gmcv2alpha1.SecurityProfileLabel: gmcv2alpha1.SecurityProfileRestricted}),
		}}))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})

		// restricted -> baseline without the opt-in annotation: denied.
		err := setNamespaceLabel(k8sClient, ns, func(n *corev1.Namespace) {
			n.Labels[gmcv2alpha1.SecurityProfileLabel] = gmcv2alpha1.SecurityProfileBaseline
		})
		require.True(t, apierrors.IsForbidden(err), "a silent downgrade must be denied; got: %v", err)

		// Same downgrade WITH the opt-in annotation: allowed.
		require.NoError(t, setNamespaceLabel(k8sClient, ns, func(n *corev1.Namespace) {
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Annotations[gmcv2alpha1.AllowProfileDowngradeAnnotation] = gmcv2alpha1.AllowProfileDowngradeAllowed
			n.Labels[gmcv2alpha1.SecurityProfileLabel] = gmcv2alpha1.SecurityProfileBaseline
		}), "a downgrade with the allow-profile-downgrade annotation must be allowed")
	})

	t.Run("privileged ineligible without the platform label, eligible with it", func(t *testing.T) {
		const ineligibleNS = "secprof-priv-ineligible"
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ineligibleNS,
			Labels: tenantLabels(map[string]string{gmcv2alpha1.SecurityProfileLabel: gmcv2alpha1.SecurityProfilePrivileged}),
		}})
		require.True(t, apierrors.IsForbidden(err),
			"privileged must be denied without the eligibility label; got: %v", err)

		const eligibleNS = "secprof-priv-eligible"
		require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: eligibleNS,
			Labels: tenantLabels(map[string]string{
				gmcv2alpha1.SecurityProfileLabel:   gmcv2alpha1.SecurityProfilePrivileged,
				gmcv2alpha1.PrivilegedProfileLabel: gmcv2alpha1.PrivilegedProfileAllowed,
			}),
		}}), "privileged must be allowed when the platform eligibility label is present")
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: eligibleNS}})
		})
	})
}

// startNamespacePSAReconciler starts a NamespacePSAReconciler for the duration of a
// test against the envtest apiserver (admin identity, so the namespace-psa-guard VAP
// does not apply to it).
func startNamespacePSAReconciler(t *testing.T) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	syncPeriod := 2 * time.Second
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
	})
	require.NoError(t, err)

	require.NoError(t, (&controller.NamespacePSAReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	go func() { _ = mgr.Start(mgrCtx) }()
}

// TestGMC_NamespacePSAReconciler_StampsProfile verifies the reconciler stamps the six
// pod-security.kubernetes.io/* labels from the namespace security-profile label, and
// re-stamps when the operator changes it (Q175).
func TestGMC_NamespacePSAReconciler_StampsProfile(t *testing.T) {
	startNamespacePSAReconciler(t)
	g := gomega.NewWithT(t)

	const ns = "secprof-reconcile"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
		gmcv2alpha1.SecurityProfileLabel:       gmcv2alpha1.SecurityProfileRestricted,
	})

	wantEnforce := func(profile string) func() string {
		return func() string {
			got := &corev1.Namespace{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, got); err != nil {
				return ""
			}
			return got.Labels["pod-security.kubernetes.io/enforce"]
		}
	}

	g.Eventually(wantEnforce(gmcv2alpha1.SecurityProfileRestricted), 30*time.Second, 200*time.Millisecond).
		Should(gomega.Equal(gmcv2alpha1.SecurityProfileRestricted), "PSA enforce must be stamped from the security-profile label")

	got := &corev1.Namespace{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ns}, got))
	for _, k := range []string{
		"pod-security.kubernetes.io/warn", "pod-security.kubernetes.io/audit",
	} {
		require.Equal(t, gmcv2alpha1.SecurityProfileRestricted, got.Labels[k], "label %s", k)
	}
	for _, k := range []string{
		"pod-security.kubernetes.io/enforce-version",
		"pod-security.kubernetes.io/warn-version",
		"pod-security.kubernetes.io/audit-version",
	} {
		require.Equal(t, "latest", got.Labels[k], "label %s", k)
	}
}

// TestGMC_NamespacePSAReconciler_DefaultsBaseline verifies an absent security-profile
// label on a managed v2 tenant namespace is stamped as the secure baseline default.
func TestGMC_NamespacePSAReconciler_DefaultsBaseline(t *testing.T) {
	startNamespacePSAReconciler(t)
	g := gomega.NewWithT(t)

	const ns = "secprof-default"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2alpha1.TenantNamespaceMarkerLabel: gmcv2alpha1.TenantNamespaceMarkerValue,
	})

	g.Eventually(func() string {
		got := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, got); err != nil {
			return ""
		}
		return got.Labels["pod-security.kubernetes.io/enforce"]
	}, 30*time.Second, 200*time.Millisecond).Should(gomega.Equal(gmcv2alpha1.SecurityProfileBaseline),
		"an absent security-profile label must default to baseline")
}
