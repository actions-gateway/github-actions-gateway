//go:build integration

package integration_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resourceGuardManifestPath is the real shipped policy — loading it (rather than
// reconstructing the objects in Go) means a CEL typo or a wrong matchCondition in
// the deployed artifact is caught here.
const resourceGuardManifestPath = "../../../config/admission-policy/tenant-resource-guard.yaml"

// installTenantResourceGuard applies the real VAP + binding and grants the GMC SA
// the cluster-wide write RBAC it holds in production, so the only thing that can
// deny the GMC SA a tenant-resource write in these tests is the policy itself (not
// missing RBAC). All objects are cluster-scoped and harmless to other tests (the
// policy matches only the GMC SA username, and the envtest manager runs as the
// admin user), so they are installed idempotently and left in place.
func installTenantResourceGuard(t *testing.T) {
	t.Helper()

	f, err := os.Open(resourceGuardManifestPath)
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

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-tenant-writes"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "serviceaccounts", "services"}, Verbs: []string{"create", "update", "delete"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create", "update", "delete"}},
			{APIGroups: []string{"cilium.io"}, Resources: []string{"ciliumnetworkpolicies"}, Verbs: []string{"create", "update", "delete"}},
			{APIGroups: []string{"monitoring.coreos.com"}, Resources: []string{"servicemonitors"}, Verbs: []string{"create", "update", "delete"}},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-tenant-writes"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "test-gmc-tenant-writes"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	if err := k8sClient.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

func newSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"k": []byte("v")},
	}
}

func newDeployment(ns, name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}
}

// TestGMC_TenantResourceGuard_ConfinesWritesToMarkedNamespaces verifies Q121/Q122:
// the GMC ServiceAccount may create tenant resources (Secrets — Q121 write path,
// and workload kinds like Deployments — Q122) only in namespaces carrying the
// actions-gateway.com/tenant=managed marker. A compromised GMC therefore
// cannot write a Secret or deploy a workload into kube-system or any unmarked
// namespace.
func TestGMC_TenantResourceGuard_ConfinesWritesToMarkedNamespaces(t *testing.T) {
	installTenantResourceGuard(t)

	const unmarkedNS = "resguard-unmarked"
	const markedNS = "resguard-marked"
	const legacyNS = "resguard-legacy"

	createNamespace(t, unmarkedNS)
	createNamespaceWithLabels(t, legacyNS, map[string]string{legacyTenantMarkerLabel: "true"})

	marked := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   markedNS,
		Labels: map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue},
	}}
	require.NoError(t, k8sClient.Create(ctx, marked))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), marked) })

	gmc := newImpersonatedClient(t, gmcSANamespace, gmcSAName)
	g := gomega.NewWithT(t)

	// Gate: wait until the policy is enforced. VAP enforcement is not instantaneous
	// after the binding is created, so poll an operation we expect to be denied.
	g.Eventually(func() bool {
		err := gmc.Create(ctx, newSecret(unmarkedNS, "probe"))
		return apierrors.IsForbidden(err)
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"GMC SA must be denied creating a Secret in an unmarked namespace")

	t.Run("Secret write denied in unmarked namespace (Q121)", func(t *testing.T) {
		err := gmc.Create(ctx, newSecret(unmarkedNS, "leak"))
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
	})

	t.Run("Secret write denied in legacy-marked namespace", func(t *testing.T) {
		err := gmc.Create(ctx, newSecret(legacyNS, "leak"))
		require.True(t, apierrors.IsForbidden(err),
			"the v1 actions-gateway.github.com/tenant marker must not make a namespace a tenant; got: %v", err)
	})

	t.Run("Secret write allowed in marked namespace (Q121)", func(t *testing.T) {
		require.NoError(t, gmc.Create(ctx, newSecret(markedNS, "tenant-secret")),
			"GMC SA must be allowed to create a Secret in a marked tenant namespace")
	})

	t.Run("Deployment write denied in unmarked namespace (Q122)", func(t *testing.T) {
		err := gmc.Create(ctx, newDeployment(unmarkedNS, "rogue"))
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
	})

	t.Run("Deployment write allowed in marked namespace (Q122)", func(t *testing.T) {
		require.NoError(t, gmc.Create(ctx, newDeployment(markedNS, "agc")),
			"GMC SA must be allowed to create a Deployment in a marked tenant namespace")
	})

	t.Run("admin is not subject to the policy", func(t *testing.T) {
		// The policy matchCondition targets only the GMC SA username, so the
		// cluster-admin client can still write into an unmarked namespace freely.
		require.NoError(t, k8sClient.Create(ctx, newSecret(unmarkedNS, "admin-secret")))
	})
}

// ensureLiveNamespace creates name, first removing a copy an earlier test left
// terminating: envtest runs no namespace controller, so a deleted namespace keeps
// its finalizer and refuses every create. The cleanup removes it the same way, so
// a later test finds no namespace rather than a terminating one.
func ensureLiveNamespace(t *testing.T, name string) {
	t.Helper()
	g := gomega.NewWithT(t)
	purge := func() {
		key := types.NamespacedName{Name: name}
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(context.Background(), key, ns); err != nil {
			return
		}
		_ = k8sClient.Delete(context.Background(), ns)
		// Re-read: the delete changed the object, and finalize is refused on a
		// stale resourceVersion.
		if err := k8sClient.Get(context.Background(), key, ns); err != nil {
			return
		}
		ns.Spec.Finalizers = nil
		_ = k8sClient.SubResource("finalize").Update(context.Background(), ns)
		g.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, &corev1.Namespace{}))
		}, 10*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), "namespace %s must be gone", name)
	}
	purge()
	require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	t.Cleanup(purge)
}

func newConfigMap(ns, name string, labels map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Data:       map[string]string{"k": "v"},
	}
}

// newAddOnObject is a minimal object of an optional add-on kind the GMC writes
// (a CNI-native policy, a ServiceMonitor), against the suite's stub CRDs.
func newAddOnObject(gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// TestGMC_TenantResourceGuard_ConfinesConfigMapAndAddOnWrites pins Q1321: the
// guard matches the kinds the GMC ClusterRole gained write grants on after Q122,
// so ConfigMaps, CNI-native policies and ServiceMonitors are confined to marked
// tenant namespaces like the original provisioning kinds. The two ConfigMap
// exceptions are each admitted, and each denial case breaks one of their
// conditions.
func TestGMC_TenantResourceGuard_ConfinesConfigMapAndAddOnWrites(t *testing.T) {
	installTenantResourceGuard(t)

	const unmarkedNS = "resguard-cm-unmarked"
	const markedNS = "resguard-cm-marked"
	createNamespace(t, unmarkedNS)
	createNamespaceWithLabels(t, markedNS, map[string]string{gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue})
	ensureLiveNamespace(t, gmcSANamespace)

	const tenantDenial = "is not a managed tenant namespace"
	shareLabels := map[string]string{
		"app.kubernetes.io/managed-by": "actions-gateway-gmc",
		"actions-gateway/proxy-share":  "true",
	}

	gmc := newImpersonatedClient(t, gmcSANamespace, gmcSAName)
	g := gomega.NewWithT(t)
	g.Eventually(func() bool {
		return apierrors.IsForbidden(gmc.Create(ctx, newConfigMap(unmarkedNS, "probe", nil)))
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"GMC SA must be denied creating a ConfigMap in an unmarked namespace")

	requireTenantDenial := func(t *testing.T, err error) {
		t.Helper()
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
		require.ErrorContains(t, err, tenantDenial, "denied by the guard's tenant rule")
	}
	// adminCreated creates obj as the admin, outside the guard's reach.
	adminCreated := func(t *testing.T, obj client.Object) client.Object {
		t.Helper()
		require.NoError(t, k8sClient.Create(ctx, obj))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
		return obj
	}

	t.Run("ConfigMap create denied in unmarked namespace", func(t *testing.T) {
		requireTenantDenial(t, gmc.Create(ctx, newConfigMap(unmarkedNS, "rogue", nil)))
	})

	t.Run("share-labelled ConfigMap create denied in unmarked namespace", func(t *testing.T) {
		requireTenantDenial(t, gmc.Create(ctx, newConfigMap(unmarkedNS, "rogue-share", shareLabels)))
	})

	t.Run("ConfigMap write allowed in marked namespace", func(t *testing.T) {
		cm := newConfigMap(markedNS, "proxy-share-provider-pool", shareLabels)
		require.NoError(t, gmc.Create(ctx, cm), "a share projection lands in a tenant namespace")
		cm.Data["k"] = "v2"
		require.NoError(t, gmc.Update(ctx, cm))
		require.NoError(t, gmc.Delete(ctx, cm))
	})

	t.Run("ConfigMap write allowed in the install namespace", func(t *testing.T) {
		cm := newConfigMap(gmcSANamespace, "actions-gateway-gmc-ip-ranges", nil)
		require.NoError(t, gmc.Create(ctx, cm), "the IP-range store lives beside the GMC")
		cm.Data["k"] = "v2"
		require.NoError(t, gmc.Update(ctx, cm))
		require.NoError(t, gmc.Delete(ctx, cm))
	})

	t.Run("Secret write still denied in the install namespace", func(t *testing.T) {
		requireTenantDenial(t, gmc.Create(ctx, newSecret(gmcSANamespace, "leak")))
	})

	t.Run("own share projection delete admitted in unmarked namespace", func(t *testing.T) {
		cm := adminCreated(t, newConfigMap(unmarkedNS, "revoked-share", shareLabels))
		require.NoError(t, gmc.Delete(ctx, cm), "a revoked share is withdrawn from an ex-tenant namespace")
	})

	t.Run("update of a share projection denied in unmarked namespace", func(t *testing.T) {
		cm := adminCreated(t, newConfigMap(unmarkedNS, "stale-share", shareLabels)).(*corev1.ConfigMap)
		cm.Data["k"] = "v2"
		requireTenantDenial(t, gmc.Update(ctx, cm))
	})

	t.Run("unlabelled ConfigMap delete denied in unmarked namespace", func(t *testing.T) {
		requireTenantDenial(t, gmc.Delete(ctx, adminCreated(t, newConfigMap(unmarkedNS, "someone-elses", nil))))
	})

	t.Run("ConfigMap with only the share label delete denied", func(t *testing.T) {
		labels := map[string]string{"actions-gateway/proxy-share": "true"}
		requireTenantDenial(t, gmc.Delete(ctx, adminCreated(t, newConfigMap(unmarkedNS, "half-labelled", labels))))
	})

	t.Run("ConfigMap with the share label not 'true' delete denied", func(t *testing.T) {
		labels := map[string]string{
			"app.kubernetes.io/managed-by": "actions-gateway-gmc",
			"actions-gateway/proxy-share":  "false",
		}
		requireTenantDenial(t, gmc.Delete(ctx, adminCreated(t, newConfigMap(unmarkedNS, "not-a-share", labels))))
	})

	addOns := map[string]schema.GroupVersionKind{
		"CiliumNetworkPolicy": {Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
		"ServiceMonitor":      {Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
	}
	for kind, gvk := range addOns {
		t.Run(kind+" write denied in unmarked namespace", func(t *testing.T) {
			requireTenantDenial(t, gmc.Create(ctx, newAddOnObject(gvk, unmarkedNS, "rogue")))
		})
		t.Run(kind+" write allowed in marked namespace", func(t *testing.T) {
			require.NoError(t, gmc.Create(ctx, newAddOnObject(gvk, markedNS, "tenant")))
		})
	}
}

// readerBindingIn is a RoleBinding named like the GMC's Secret reader binding, for
// the guard's exception tests to vary one field of at a time.
func readerBindingIn(ns, name string, ref rbacv1.RoleRef, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		RoleRef:    ref,
		Subjects:   subjects,
	}
}

// TestGMC_TenantResourceGuard_AdmitsOnlyOwnReaderBindingDelete pins Q1271's
// exception: outside a tenant namespace the GMC may delete its own
// gmc-tenant-secret-reader RoleBinding and nothing else. Each denial case breaks
// exactly one of the exception's conditions, and asserts the guard's tenant
// message so a denial from RBAC or a CEL error cannot pass for it.
func TestGMC_TenantResourceGuard_AdmitsOnlyOwnReaderBindingDelete(t *testing.T) {
	installTenantResourceGuard(t)
	installGMCShippedRBAC(t)

	// The GMC holds no grant on Roles; give it delete so only the guard can refuse.
	roleDeletes := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-role-deletes"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"roles"}, Verbs: []string{"delete"}}},
	}
	if err := k8sClient.Create(ctx, roleDeletes); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	roleDeletesBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-role-deletes"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleDeletes.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	if err := k8sClient.Create(ctx, roleDeletesBinding); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	const ns = "resguard-reader-cleanup"
	createNamespace(t, ns)

	const reader = "gmc-tenant-secret-reader"
	readerRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: reader}
	gmcSubject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: gmcSAName, Namespace: gmcSANamespace}
	const tenantDenial = "is not a managed tenant namespace"

	gmc := newImpersonatedClient(t, gmcSANamespace, gmcSAName)
	g := gomega.NewWithT(t)
	g.Eventually(func() bool {
		return apierrors.IsForbidden(gmc.Create(ctx, newSecret(ns, "probe")))
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), "the guard must be enforcing")

	// deleteDenied creates obj as the admin, then requires the guard to deny the
	// GMC's delete of it.
	deleteDenied := func(t *testing.T, obj client.Object) {
		t.Helper()
		require.NoError(t, k8sClient.Create(ctx, obj))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
		err := gmc.Delete(ctx, obj)
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
		require.ErrorContains(t, err, tenantDenial, "denied by the guard's tenant rule")
	}

	t.Run("own reader binding delete admitted", func(t *testing.T) {
		rb := readerBindingIn(ns, reader, readerRef, gmcSubject)
		require.NoError(t, k8sClient.Create(ctx, rb))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rb) })
		require.NoError(t, gmc.Delete(ctx, rb), "the GMC removes its own reader binding outside a tenant namespace")
	})

	t.Run("update of own reader binding denied", func(t *testing.T) {
		rb := readerBindingIn(ns, reader, readerRef, gmcSubject)
		require.NoError(t, k8sClient.Create(ctx, rb))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rb) })
		rb.Labels = map[string]string{"touched": "true"}
		err := gmc.Update(ctx, rb)
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
		require.ErrorContains(t, err, tenantDenial)
	})

	t.Run("binding with another name denied", func(t *testing.T) {
		deleteDenied(t, readerBindingIn(ns, "not-the-reader", readerRef, gmcSubject))
	})

	t.Run("binding to another ClusterRole denied", func(t *testing.T) {
		deleteDenied(t, readerBindingIn(ns, reader, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}, gmcSubject))
	})

	t.Run("binding to a Role of the reader's name denied", func(t *testing.T) {
		deleteDenied(t, readerBindingIn(ns, reader, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: reader}, gmcSubject))
	})

	t.Run("binding with an extra subject denied", func(t *testing.T) {
		extra := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "someone-else", Namespace: gmcSANamespace}
		deleteDenied(t, readerBindingIn(ns, reader, readerRef, gmcSubject, extra))
	})

	t.Run("binding to another ServiceAccount denied", func(t *testing.T) {
		other := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "someone-else", Namespace: gmcSANamespace}
		deleteDenied(t, readerBindingIn(ns, reader, readerRef, other))
	})

	t.Run("binding to a User named like the GMC denied", func(t *testing.T) {
		user := rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: gmcSAName, Namespace: gmcSANamespace}
		deleteDenied(t, readerBindingIn(ns, reader, readerRef, user))
	})

	t.Run("Role of the reader's name denied", func(t *testing.T) {
		deleteDenied(t, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: reader}})
	})

	t.Run("ClusterRoleBinding of the reader's name denied", func(t *testing.T) {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: reader},
			RoleRef:    readerRef,
			Subjects:   []rbacv1.Subject{gmcSubject},
		}
		require.NoError(t, k8sClient.Create(ctx, crb))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), crb) })
		err := gmc.Delete(ctx, crb)
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
		require.ErrorContains(t, err, "agc-clusterrunnertemplate-reader", "denied by the guard's ClusterRoleBinding rule")
	})
}
