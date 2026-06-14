//go:build integration

package integration_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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
			{APIGroups: []string{""}, Resources: []string{"secrets", "serviceaccounts", "services"}, Verbs: []string{"create", "update", "delete"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create", "update", "delete"}},
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
// actions-gateway.github.com/tenant=true marker. A compromised GMC therefore
// cannot write a Secret or deploy a workload into kube-system or any unmarked
// namespace.
func TestGMC_TenantResourceGuard_ConfinesWritesToMarkedNamespaces(t *testing.T) {
	installTenantResourceGuard(t)

	const unmarkedNS = "resguard-unmarked"
	const markedNS = "resguard-marked"

	createNamespace(t, unmarkedNS)

	marked := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   markedNS,
		Labels: map[string]string{controller.TenantNamespaceMarkerLabel: "true"},
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
