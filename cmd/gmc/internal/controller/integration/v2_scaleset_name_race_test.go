//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// scaleSetRaceIterations bounds the concurrent-admission attempts in
// TestV2_ScaleSetNameGuard_ConcurrentPairIsReported. Measured 2026-11-11 on this
// suite: 25 of 50 concurrent pairs were both admitted.
const scaleSetRaceIterations = 50

// TestV2_ScaleSetNameGuard_ConcurrentPairIsReported pins the bound on Q1325's third
// lead, an accepted residual (05-security.md). The scale-set name guard arbitrates a
// pair from reads of what is already stored, so a RunnerSet and its ActionsGateway
// created at once can each see the pair incomplete and both be admitted onto an
// established tenant's name. The established gateway must then report the collision,
// and a raced gateway's finalizer write, which the GMC needs before it provisions
// anything, must be refused while the pair collides. The test logs how many raced
// gateways got the finalizer anyway, from a reconcile landing between the two creates.
func TestV2_ScaleSetNameGuard_ConcurrentPairIsReported(t *testing.T) {
	const (
		nsA    = "v2-ssname-race-a"
		nsB    = "v2-ssname-race-b"
		acme   = "https://github.com/acme-race"
		shared = "shared-race"
	)
	createNamespace(t, nsA)
	createNamespace(t, nsB)
	createGitHubAppSecret(t, nsA, "github-app")
	createGitHubAppSecret(t, nsB, "github-app")
	cleanup := func(obj client.Object) {
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)) })
	}
	// The held tenant is established: its gateway already carries the GMC finalizer.
	startActionsGatewayV2Reconciler(t)
	for _, obj := range []client.Object{
		ghesAdmissionGateway("gw", nsA, acme, ""),
		newScaleSetRunnerSet("held", nsA, "gw", shared),
	} {
		require.NoError(t, k8sClient.Create(ctx, obj))
		cleanup(obj)
	}
	hasFinalizer := func(key types.NamespacedName) bool {
		var got v2.ActionsGateway
		return k8sClient.Get(ctx, key, &got) == nil &&
			controllerutil.ContainsFinalizer(&got, v2.ActionsGatewayFinalizer)
	}
	heldKey := types.NamespacedName{Namespace: nsA, Name: "gw"}
	require.Eventually(t, func() bool { return hasFinalizer(heldKey) },
		30*time.Second, 100*time.Millisecond, "the held gateway must be reconciled before the race")

	var raced []types.NamespacedName
	for i := range scaleSetRaceIterations {
		gwB := ghesAdmissionGateway(fmt.Sprintf("gw-%d", i), nsB, acme, "")
		rsB := newScaleSetRunnerSet(fmt.Sprintf("racer-%d", i), nsB, gwB.Name, shared)
		var gwErr, rsErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; gwErr = k8sClient.Create(ctx, gwB) }()
		go func() { defer wg.Done(); <-start; rsErr = k8sClient.Create(ctx, rsB) }()
		close(start)
		wg.Wait()
		cleanup(gwB)
		cleanup(rsB)
		if gwErr == nil && rsErr == nil {
			raced = append(raced, client.ObjectKeyFromObject(gwB))
		}
	}
	t.Logf("%d of %d concurrent pairs were both admitted", len(raced), scaleSetRaceIterations)
	if len(raced) == 0 {
		// The report itself is pinned by TestV2_ActionsGateway_ScaleSetNameCollision;
		// this run only failed to produce the state it reports on.
		t.Skipf("no concurrent pair was admitted in %d iterations", scaleSetRaceIterations)
	}

	var last []metav1.Condition
	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, heldKey, &got); err != nil {
			return false
		}
		last = got.Status.Conditions
		c := findCondition(got.Status.Conditions, v2.ConditionScaleSetNameCollision)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2.ReasonScaleSetNameShared
	}, 30*time.Second, 100*time.Millisecond,
		"the held gateway must report the raced pair as ScaleSetNameShared; conditions: %+v", &last)

	// A raced gateway's first GMC write, the finalizer, is an update the gateway
	// webhook refuses while the pair collides, and provisioning starts only after it.
	// That refusal is the bound, so it is asserted, not only observed: an exemption
	// for metadata-only updates would let the GMC provision a raced gateway (Q1342).
	provisioned := 0
	for _, key := range raced {
		if hasFinalizer(key) {
			provisioned++
			continue
		}
		var got v2.ActionsGateway
		require.NoError(t, k8sClient.Get(ctx, key, &got))
		controllerutil.AddFinalizer(&got, v2.ActionsGatewayFinalizer)
		require.Error(t, k8sClient.Update(ctx, &got),
			"%s: the finalizer write that starts provisioning must be refused while the pair collides", key)
	}
	t.Logf("%d of %d raced gateways carry the GMC finalizer, so may have been provisioned", provisioned, len(raced))
}
