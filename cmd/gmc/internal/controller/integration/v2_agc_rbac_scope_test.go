//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_AGCRole_ScopedToItsOwnNamespace asks the apiserver what a v2 gateway's AGC
// ServiceAccount may do, by impersonating it: the per-gateway RoleBinding to
// agc-tenant-role grants its own namespace and nothing else, and the serviceaccounts
// grant is get-only (Q1066). TestV2_ActionsGateway_ProvisionsAGCControlPlane asserts
// the RoleBinding exists; this asserts what it permits.
func TestV2_AGCRole_ScopedToItsOwnNamespace(t *testing.T) {
	const (
		nsA = "v2-rbac-a"
		nsB = "v2-rbac-b"
		gw  = "gw"
	)
	for _, ns := range []string{nsA, nsB} {
		createNamespace(t, ns)
		createGitHubAppSecret(t, ns, "github-app")
		ag := newV2GatewayWired(gw, ns, "github-app", "")
		require.NoError(t, k8sClient.Create(ctx, ag))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), ag)
			_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + ns + "." + gw}})
		})
	}
	startActionsGatewayV2Reconciler(t)

	for _, ns := range []string{nsA, nsB} {
		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gw + "-agc"}, &rbacv1.RoleBinding{}) == nil &&
				k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gw + "-worker"}, &corev1.ServiceAccount{}) == nil
		}, 15*time.Second, 100*time.Millisecond, "the RoleBinding and worker ServiceAccount in %s", ns)
	}

	agc := newImpersonatedClient(t, nsA, gw+"-agc")

	// The binding reaches the authorizer asynchronously, so the allowed read is retried;
	// every denial below is then read against a settled binding.
	require.Eventually(t, func() bool {
		return agc.Get(ctx, types.NamespacedName{Namespace: nsA, Name: gw + "-worker"}, &corev1.ServiceAccount{}) == nil
	}, 15*time.Second, 100*time.Millisecond, "the AGC reads its own worker ServiceAccount")
	assert.NoError(t, agc.List(ctx, &corev1.SecretList{}, client.InNamespace(nsA)),
		"the AGC lists Secrets in its own namespace")

	err := agc.List(ctx, &corev1.SecretList{}, client.InNamespace(nsB))
	assert.True(t, apierrors.IsForbidden(err), "listing another tenant's Secrets must be forbidden; got: %v", err)

	err = agc.Get(ctx, types.NamespacedName{Namespace: nsB, Name: gw + "-worker"}, &corev1.ServiceAccount{})
	assert.True(t, apierrors.IsForbidden(err), "another tenant's worker ServiceAccount must be forbidden; got: %v", err)

	err = agc.List(ctx, &corev1.ServiceAccountList{}, client.InNamespace(nsA))
	assert.True(t, apierrors.IsForbidden(err), "listing ServiceAccounts must be forbidden even in its own namespace; got: %v", err)

	err = agc.List(ctx, &v2.ActionsGatewayList{}, client.InNamespace(nsB))
	assert.True(t, apierrors.IsForbidden(err), "listing another tenant's ActionsGateways must be forbidden; got: %v", err)
}
