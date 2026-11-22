//go:build integration

package integration_test

import (
	"strings"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGMC_NamespacePSAReconciler_ReclaimsLabelsFromAnotherManager has a second field
// manager take the enforce label with a forced server-side apply, the way an admin's
// out-of-band edit or a 1.x GMC's field manager holds it. The reconciler's own apply
// then conflicts, which only a real apiserver produces, and its ForceOwnership retry
// must restore the profile and take the key back.
func TestGMC_NamespacePSAReconciler_ReclaimsLabelsFromAnotherManager(t *testing.T) {
	startNamespacePSAReconciler(t)

	const ns = "secprof-conflict"
	const enforceKey = "pod-security.kubernetes.io/enforce"
	const otherManager = "q1259-admin"
	createNamespaceWithLabels(t, ns, map[string]string{
		gmcv2.TenantNamespaceMarkerLabel: gmcv2.TenantNamespaceMarkerValue,
		gmcv2.SecurityProfileLabel:       gmcv2.SecurityProfileRestricted,
	})

	// reclaimed reports whether enforce carries the profile and otherManager owns no
	// part of it.
	reclaimed := func() bool {
		var got corev1.Namespace
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &got); err != nil {
			return false
		}
		if got.Labels[enforceKey] != gmcv2.SecurityProfileRestricted {
			return false
		}
		for _, mf := range got.ManagedFields {
			if mf.Manager == otherManager && mf.FieldsV1 != nil && strings.Contains(mf.FieldsV1.GetRawString(), `"f:`+enforceKey+`"`) {
				return false
			}
		}
		return true
	}
	require.Eventually(t, reclaimed, 30*time.Second, 200*time.Millisecond, "the profile should be stamped first")

	require.NoError(t, k8sClient.Apply(ctx,
		corev1ac.Namespace(ns).WithLabels(map[string]string{enforceKey: gmcv2.SecurityProfileBaseline}),
		client.FieldOwner(otherManager), client.ForceOwnership))

	require.Eventually(t, reclaimed, 30*time.Second, 200*time.Millisecond,
		"the reconciler must take enforce back from another field manager")
}
