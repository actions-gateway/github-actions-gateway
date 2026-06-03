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
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gmcSAName / gmcSANamespace are the default install identity the namespace-psa-guard
// ValidatingAdmissionPolicy scopes to (must match the matchCondition username in
// config/admission-policy/namespace-psa-guard.yaml).
const (
	gmcSANamespace = "gmc-system"
	gmcSAName      = "gmc-controller-manager"
)

// vapManifestPath is the real shipped policy — loading it (rather than reconstructing
// the objects in Go) means a CEL typo or a wrong matchCondition in the deployed
// artifact is caught here.
const vapManifestPath = "../../../config/admission-policy/namespace-psa-guard.yaml"

// installNamespacePSAGuard applies the real VAP + binding and grants the GMC SA the
// cluster-wide namespace RBAC it holds in production, so the only thing that can deny
// the GMC SA a namespace patch in these tests is the policy itself (not missing RBAC).
// All objects are cluster-scoped and harmless to other tests (the policy matches only
// the GMC SA username, and the envtest manager runs as the admin user), so they are
// installed idempotently and left in place.
func installNamespacePSAGuard(t *testing.T) {
	t.Helper()

	f, err := os.Open(vapManifestPath)
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
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-namespace-patch"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gmc-namespace-patch"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "test-gmc-namespace-patch"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: gmcSAName, Namespace: gmcSANamespace}},
	}
	if err := k8sClient.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// setNamespaceLabel reads the namespace with c and applies the mutation, returning
// the Update error so callers can assert on admission denials.
func setNamespaceLabel(c client.Client, name string, mutate func(*corev1.Namespace)) error {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		return err
	}
	mutate(ns)
	return c.Update(ctx, ns)
}

// TestGMC_NamespacePSAGuard_EnforcesMarkerAndFieldScope verifies finding B2 / Q56:
// the GMC ServiceAccount may patch a namespace only if it is a marked tenant
// namespace, and only the six pod-security.kubernetes.io/* labels.
func TestGMC_NamespacePSAGuard_EnforcesMarkerAndFieldScope(t *testing.T) {
	installNamespacePSAGuard(t)

	const unmarkedNS = "psa-guard-unmarked"
	const markedNS = "psa-guard-marked"

	createNamespace(t, unmarkedNS)

	// Marked tenant namespace — the marker is applied by the admin client (the
	// trusted actor), mirroring the onboarding step. The GMC never sets it.
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
		err := setNamespaceLabel(gmc, unmarkedNS, func(ns *corev1.Namespace) {
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
		})
		return apierrors.IsForbidden(err)
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"GMC SA must be denied patching an unmarked namespace")

	t.Run("marked namespace PSA labels allowed", func(t *testing.T) {
		err := setNamespaceLabel(gmc, markedNS, func(ns *corev1.Namespace) {
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
			ns.Labels["pod-security.kubernetes.io/enforce-version"] = "latest"
		})
		require.NoError(t, err, "GMC SA must be allowed to set PSA labels on a marked tenant namespace")
	})

	t.Run("non-PSA label change on marked namespace denied", func(t *testing.T) {
		err := setNamespaceLabel(gmc, markedNS, func(ns *corev1.Namespace) {
			ns.Labels["example.com/not-psa"] = "x"
		})
		require.True(t, apierrors.IsForbidden(err),
			"GMC SA must not change non-PSA labels; got: %v", err)
	})

	t.Run("annotation change on marked namespace denied", func(t *testing.T) {
		err := setNamespaceLabel(gmc, markedNS, func(ns *corev1.Namespace) {
			if ns.Annotations == nil {
				ns.Annotations = map[string]string{}
			}
			ns.Annotations["example.com/note"] = "x"
		})
		require.True(t, apierrors.IsForbidden(err),
			"GMC SA must not change annotations; got: %v", err)
	})

	t.Run("admin is not subject to the policy", func(t *testing.T) {
		// The policy matchCondition targets only the GMC SA username, so the
		// cluster-admin client can still patch an unmarked namespace freely.
		err := setNamespaceLabel(k8sClient, unmarkedNS, func(ns *corev1.Namespace) {
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels["example.com/admin-edit"] = "ok"
		})
		require.NoError(t, err)
	})
}
