//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

// startGMCAsServiceAccount runs the gateway reconciler and the tenant Secret
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
	require.NoError(t, (&controller.ActionsGatewayV2Reconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		AGCImage:      "agc:test",
		Recorder:      mgr.GetEventRecorder("actionsgateway-v2-controller"),
		Reader:        mgr.GetAPIReader(),
		SecretWatches: watches,
	}).SetupWithManager(mgr))

	go func() { _ = mgr.Start(mgrCtx) }()
}

func credentialUnavailable(t *testing.T, ns, name string) func() bool {
	return func() bool {
		var ag gmcv2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ag); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(ag.Status.Conditions, gmcv2.ConditionCredentialUnavailable)
	}
}

// TestGMC_TenantSecretAccess_ScopesReadsToTenantNamespaces pins Q1188: the GMC,
// running under its shipped RBAC, provisions a tenant gateway and reacts to its
// credential Secret appearing and disappearing, while it cannot read a Secret in
// an unmarked namespace, list Secrets cluster-wide, or bind its reader there. The
// unmarked namespace carries the v1 marker, which grants nothing.
func TestGMC_TenantSecretAccess_ScopesReadsToTenantNamespaces(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const tenantNS = "secret-access-tenant"
	const otherNS = "secret-access-unmarked"
	createNamespaceWithLabels(t, tenantNS, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	createNamespaceWithLabels(t, otherNS, map[string]string{legacyTenantMarkerLabel: "true"})
	require.NoError(t, k8sClient.Create(ctx, newSecret(otherNS, "not-the-gmcs")))

	ag := newV2GatewayWired("secret-access-gw", tenantNS, "github-app", "")
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: ag.Name + controller.AGCResourceSuffix}, &appsv1.Deployment{})
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

	// RBAC lets the GMC's `bind` grants back a ClusterRoleBinding too, which would
	// grant Secret reads in every namespace; the guard admits only the
	// ClusterRunnerTemplate reader binding.
	crb := func(name, role string) *rbacv1.ClusterRoleBinding {
		return &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}},
		}
	}
	for _, role := range []string{controller.TenantSecretReaderName, "agc-tenant-role"} {
		err = gmc.Create(ctx, crb("secret-access-escalation-"+role, role))
		require.True(t, apierrors.IsForbidden(err), "a ClusterRoleBinding to %s must be denied; got: %v", role, err)
		require.ErrorContains(t, err, "agc-clusterrunnertemplate-reader", "denied by the guard, not by RBAC")
	}
	err = gmc.List(ctx, &corev1.SecretList{})
	require.True(t, apierrors.IsForbidden(err), "Secrets stay unlistable cluster-wide; got: %v", err)

	allowed := crb("secret-access-crt-reader", "agc-clusterrunnertemplate-reader")
	require.NoError(t, gmc.Create(ctx, allowed), "the per-gateway ClusterRunnerTemplate reader binding stays writable")
	require.NoError(t, gmc.Delete(ctx, allowed))
}

// TestGMC_TenantSecretAccess_DeletesReaderBindingOutsideTenants pins Q1271: the
// GMC, under its shipped RBAC and the real guard, removes its reader binding from
// a namespace that stops being a tenant. The legacy namespace stands in for the
// v2.0.0 upgrade: a 1.x GMC bound its reader there, and only the v1 marker
// remains.
func TestGMC_TenantSecretAccess_DeletesReaderBindingOutsideTenants(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const tenantNS = "reader-cleanup-tenant"
	const legacyNS = "reader-cleanup-legacy"
	createNamespaceWithLabels(t, tenantNS, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	createNamespaceWithLabels(t, legacyNS, map[string]string{legacyTenantMarkerLabel: "true"})
	leftover := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNS, Name: controller.TenantSecretReaderName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: controller.TenantSecretReaderName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	require.NoError(t, k8sClient.Create(ctx, leftover))

	startGMCAsServiceAccount(t)
	g := gomega.NewWithT(t)
	bindingGone := func(ns string) func() bool {
		return func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
			return apierrors.IsNotFound(err)
		}
	}

	g.Eventually(bindingGone(legacyNS), 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"the GMC removes a 1.x reader binding from a namespace carrying only the v1 marker")

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var ns corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &ns))
	delete(ns.Labels, gmcv2.TenantNamespaceMarkerLabel)
	require.NoError(t, k8sClient.Update(ctx, &ns))
	g.Eventually(bindingGone(tenantNS), 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"unmarking a tenant namespace removes the GMC's reader binding")
}

// TestGMC_TenantSecretAccess_UnmarkRevokesEveryGMCBinding pins Q1328: unmarking a
// tenant namespace has to leave the GMC with no Secret read there. The unmark
// removes one binding by name, so the guard must stop the GMC writing any other
// RoleBinding that would grant it Secret reads: a second binding to its reader, or
// one to agc-tenant-role naming its own ServiceAccount.
func TestGMC_TenantSecretAccess_UnmarkRevokesEveryGMCBinding(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const tenantNS = "reader-unmark-tenant"
	createNamespaceWithLabels(t, tenantNS, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	require.NoError(t, k8sClient.Create(ctx, newSecret(tenantNS, "dummy")))

	startGMCAsServiceAccount(t)
	g := gomega.NewWithT(t)
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	gmc, err := client.New(gmcServiceAccountConfig(), client.Options{Scheme: testScheme})
	require.NoError(t, err)
	self := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}}
	for name, role := range map[string]string{"extra-reader": controller.TenantSecretReaderName, "extra-agc": "agc-tenant-role"} {
		err := gmc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   self,
		})
		require.True(t, apierrors.IsForbidden(err), "a GMC RoleBinding %s to %s naming itself must be denied; got: %v", name, role, err)
		require.ErrorContains(t, err, "may only write its own gmc-tenant-secret-reader RoleBinding", "denied by the guard, not by RBAC")
	}
	agcBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNS, Name: "agc-shape"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "agc-tenant-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "some-agc", Namespace: tenantNS}},
	}
	require.NoError(t, gmc.Create(ctx, agcBinding), "the AGC binding shape stays writable")
	require.NoError(t, gmc.Delete(ctx, agcBinding))

	var ns corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &ns))
	delete(ns.Labels, gmcv2.TenantNamespaceMarkerLabel)
	require.NoError(t, k8sClient.Update(ctx, &ns))
	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: controller.TenantSecretReaderName}, &rbacv1.RoleBinding{})
		return apierrors.IsNotFound(err)
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue())

	g.Eventually(func() bool {
		err := gmc.Get(ctx, types.NamespacedName{Namespace: tenantNS, Name: "dummy"}, &corev1.Secret{})
		return apierrors.IsForbidden(err)
	}, 5*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "an ex-tenant namespace must grant the GMC no Secret read")
}

// TestGMC_Teardown_HoldsWorkloadPolicyForWorkerPod pins Q1328 under the GMC's
// shipped RBAC: deleting a gateway keeps its workload NetworkPolicy, and its
// finalizer, while a worker pod of a bound RunnerSet is still present. The
// RunnerSet reports no jobs, as it does once the AGC has issued its deletes, and a
// finalizer holds the pod in Terminating as the kubelet's grace period would.
func TestGMC_Teardown_HoldsWorkloadPolicyForWorkerPod(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const ns = "teardown-hold-tenant"
	const gwName = "gw-hold"
	createNamespaceWithLabels(t, ns, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	createGitHubAppSecret(t, ns, "github-app")
	ag := newV2GatewayWired(gwName, ns, "github-app", "")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
	rs := newBoundRunnerSet("hold-set", ns)
	rs.Spec.GatewayRef.Name = gwName
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	startGMCAsServiceAccount(t)
	g := gomega.NewWithT(t)
	npKey := types.NamespacedName{Namespace: ns, Name: gwName + "-workload"}
	g.Eventually(func() error {
		return k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{})
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed())

	const hold = "test.actions-gateway.com/kubelet-grace"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hold-set-w1", Namespace: ns, Finalizers: []string{hold},
			Labels: map[string]string{"actions-gateway/component": "workload", "actions-gateway.com/runner-set": "hold-set"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	release := func() {
		var p corev1.Pod
		if k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &p) == nil {
			p.Finalizers = nil
			_ = k8sClient.Update(context.Background(), &p)
		}
	}
	t.Cleanup(release)
	require.NoError(t, k8sClient.Delete(ctx, pod))
	require.NoError(t, k8sClient.Delete(ctx, ag))

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwName + controller.AGCResourceSuffix}, &appsv1.Deployment{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(), "the rest of the control plane is torn down")
	g.Consistently(func() error {
		return k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{})
	}, 3*time.Second, 100*time.Millisecond).Should(gomega.Succeed(), "the workload NetworkPolicy outlives a terminating worker pod")
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ag), &gmcv2.ActionsGateway{}), "the gateway is held by its finalizer")

	release()
	g.Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ag), &gmcv2.ActionsGateway{}))
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.BeTrue(), "the gateway finalizes away once the pod is gone")
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, npKey, &networkingv1.NetworkPolicy{})))
}

// TestGMC_Teardown_FinalizesInTerminatingNamespace pins the Q1328 review finding:
// deleting a tenant namespace revokes the reader binding the teardown hold lists
// Pods through, so the hold must not apply there or the gateway never finalizes.
// envtest runs no namespace controller, so the namespace's binding delete is done by
// hand and the namespace stays Terminating.
func TestGMC_Teardown_FinalizesInTerminatingNamespace(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	const ns = "teardown-ns-terminating"
	const gwName = "gw-ns-gone"
	createNamespaceWithLabels(t, ns, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	createGitHubAppSecret(t, ns, "github-app")
	ag := newV2GatewayWired(gwName, ns, "github-app", "")
	require.NoError(t, k8sClient.Create(ctx, ag))
	rs := newBoundRunnerSet("ns-gone-set", ns)
	rs.Spec.GatewayRef.Name = gwName
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	startGMCAsServiceAccount(t)
	g := gomega.NewWithT(t)
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwName + "-workload"}, &networkingv1.NetworkPolicy{})
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed())

	var n corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &n))
	require.NoError(t, k8sClient.Delete(ctx, &n))
	require.NoError(t, k8sClient.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: controller.TenantSecretReaderName}}))
	require.NoError(t, k8sClient.Delete(ctx, ag))

	g.Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ag), &gmcv2.ActionsGateway{}))
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), "the gateway finalizes in a terminating namespace")
}
