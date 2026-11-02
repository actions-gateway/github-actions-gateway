//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The reconcile that adds the finalizer returns an empty Result, so provisioning
// continues only if the finalizer Update reaches the primary watch (Q1063). The
// manager runs without the suite's 2s resync, so a predicate on For() that drops
// metadata-only updates (GenerationChangedPredicate) leaves the gateway with its
// finalizer and no AGC Deployment.
func TestV2_ActionsGateway_FinalizerUpdateDrivesNextReconcile(t *testing.T) {
	const ns = "v2-ag-finalizer-watch"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2.SecurityProfileLabel: v2.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), newV2EgressProxyObject("shared", ns))
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)
	startActionsGatewayV2ReconcilerSync(t, mgrCtx, nil, false, nil)

	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got) == nil &&
			controllerutil.ContainsFinalizer(&got, v2.ActionsGatewayFinalizer)
	}, 15*time.Second, 100*time.Millisecond, "finalizer should be added")

	require.Eventually(t, func() bool {
		var dep appsv1.Deployment
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should follow the finalizer add with no resync")
}
