//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The reconcile that adds a finalizer returns an empty Result, so the work after
// it runs only if the finalizer Update reaches the primary watch (Q1063). These
// managers keep controller-runtime's default resync, so a predicate on For() that
// drops metadata-only updates (GenerationChangedPredicate) stalls both objects
// right after the finalizer lands.

func TestAGC_RunnerGroup_FinalizerUpdateDrivesNextReconcile(t *testing.T) {
	const nsName = "agc-finalizer-watch"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "test-rg", 1)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		return k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "test-rg"},
		) == nil && len(secrets.Items) == 1
	}, 15*time.Second, 50*time.Millisecond, "agent Secret should follow the finalizer add with no resync")
}

func TestV2_RunnerSet_FinalizerUpdateDrivesNextReconcile(t *testing.T) {
	const ns = "v2-rs-finalizer-watch"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	// No gateway exists, so the only status the set can reach is GatewayNotFound,
	// written by the reconcile after the one that added the finalizer.
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonGatewayNotFound)
}
