//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reconcile that adds a finalizer returns an empty Result, so the work after
// it runs only if the finalizer Update reaches the primary watch (Q1063). These
// managers keep controller-runtime's default resync, so a predicate on For() that
// drops metadata-only updates (GenerationChangedPredicate) stalls both objects
// right after the finalizer lands.

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
