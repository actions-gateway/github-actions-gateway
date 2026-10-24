//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

// installGMCShippedRBAC grants the GMC ServiceAccount exactly the RBAC the chart
// ships: the manager ClusterRole from the generated rules fragment, and the
// gmc-tenant-secret-reader ClusterRole the GMC binds per tenant namespace. Both are
// read from charts/actions-gateway/files, so a rule the chart drops is dropped here.
func installGMCShippedRBAC(t *testing.T) {
	t.Helper()
	install := func(name, fragment string) {
		data, err := os.ReadFile(filepath.Join("testdata", "chartfiles", fragment))
		require.NoError(t, err)
		var rules []rbacv1.PolicyRule
		require.NoError(t, yaml.Unmarshal(data, &rules))
		cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}
		if err := k8sClient.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}
	install("test-gmc-manager-role", "manager-role-rules.yaml")
	install(controller.TenantSecretReaderName, "gmc-tenant-secret-reader-rules.yaml")
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-manager-role"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "test-gmc-manager-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	if err := k8sClient.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// gmcServiceAccountConfig is the envtest config impersonating the GMC
// ServiceAccount, so a manager built on it holds only the GMC's shipped RBAC.
func gmcServiceAccountConfig() *rest.Config {
	cfg := rest.CopyConfig(testEnv.Config)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + gmcSANamespace + ":" + gmcSAName,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + gmcSANamespace},
	}
	return cfg
}

// startGMCAsServiceAccount runs the v1 gateway reconciler and the tenant Secret
// access reconciler as the GMC ServiceAccount, with the production Secret
// cache bypass and the default 10h resync, so a gateway reacts to a Secret change
// within a test's window only through the per-namespace Secret watch.
func startGMCAsServiceAccount(t *testing.T) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(gmcServiceAccountConfig(), ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}},
		}},
	})
	require.NoError(t, err)

	watches := &controller.TenantSecretWatches{
		Config:     mgr.GetConfig(),
		HTTPClient: mgr.GetHTTPClient(),
		Scheme:     mgr.GetScheme(),
		Mapper:     mgr.GetRESTMapper(),
	}
	require.NoError(t, (&controller.TenantSecretAccessReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		ServiceAccountName:      gmcSAName,
		ServiceAccountNamespace: gmcSANamespace,
		Watches:                 watches,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.ActionsGatewayReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		IPCache:       &controller.IPRangeCache{},
		AGCImage:      "agc:test",
		ProxyImage:    "proxy:test",
		SecretWatches: watches,
	}).SetupWithManager(mgr))

	go func() { _ = mgr.Start(mgrCtx) }()
}

func credentialUnavailable(t *testing.T, ns, name string) func() bool {
	return func() bool {
		var ag gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ag); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(ag.Status.Conditions, gmcv1alpha1.ConditionCredentialUnavailable)
	}
}

// TestGMC_TenantSecretAccess_ScopesReadsToTenantNamespaces pins Q1188: the GMC,
// running under its shipped RBAC, provisions a tenant gateway and reacts to its
// credential Secret appearing and disappearing, while it cannot read a Secret in
// an unmarked namespace, list Secrets cluster-wide, or bind its reader there.
func TestGMC_TenantSecretAccess_ScopesReadsToTenantNamespaces(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const tenantNS = "secret-access-tenant"
	const otherNS = "secret-access-unmarked"
	createNamespaceWithLabels(t, tenantNS, map[string]string{controller.TenantNamespaceMarkerLabel: "true"})
	createNamespace(t, otherNS)
	require.NoError(t, k8sClient.Create(ctx, newSecret(otherNS, "not-the-gmcs")))

	ag := newActionsGateway("secret-access-gw", tenantNS, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCAsServiceAccount(t)
	g := gomega.NewWithT(t)

	// The reader binding lands in the tenant namespace only.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: otherNS, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
	require.True(t, apierrors.IsNotFound(err), "no reader binding outside tenant namespaces; got: %v", err)

	// The credential is missing, so the gateway fails closed.
	g.Eventually(credentialUnavailable(t, tenantNS, ag.Name), 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue())

	// Creating it reaches the gateway well inside the 30s missing-credential
	// requeue, so the event came from the tenant namespace's Secret watch.
	createGitHubAppSecret(t, tenantNS, "github-app")
	g.Eventually(credentialUnavailable(t, tenantNS, ag.Name), 15*time.Second, 25*time.Millisecond).Should(gomega.BeFalse())
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: agcName}, &appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "the gateway provisions under the shipped RBAC")

	// A healthy gateway has no short requeue and the resync is 10h, so only the
	// watch can deliver the deletion.
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: "github-app"}}))
	g.Eventually(credentialUnavailable(t, tenantNS, ag.Name), 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue())

	gmc, err := client.New(gmcServiceAccountConfig(), client.Options{Scheme: testScheme})
	require.NoError(t, err)

	err = gmc.Get(ctx, types.NamespacedName{Namespace: otherNS, Name: "not-the-gmcs"}, &corev1.Secret{})
	require.True(t, apierrors.IsForbidden(err), "a Secret outside tenant namespaces must be unreadable; got: %v", err)

	err = gmc.List(ctx, &corev1.SecretList{})
	require.True(t, apierrors.IsForbidden(err), "a cluster-wide Secret list must be denied; got: %v", err)

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: otherNS, Name: controller.TenantSecretReaderName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: controller.TenantSecretReaderName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	err = gmc.Create(ctx, rb)
	require.Error(t, err, "the GMC must not bind its reader outside tenant namespaces")
	require.False(t, apierrors.IsAlreadyExists(err), "got: %v", err)
}
