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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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

// TestV2_ScaleSetNameGuard_HeldGatewayStaysEditable covers Q1342: a stored collision
// must not freeze the established tenant's gateway against its own edits. The second
// tenant's pair is stored with admission bypassed, the state a raced pair, an upgrade
// from before Q791, or a webhook-uninstalled window leaves; the held gateway stays in
// a validated namespace, so its edit goes through the webhook.
func TestV2_ScaleSetNameGuard_HeldGatewayStaysEditable(t *testing.T) {
	const (
		nsA    = "v2-ssname-held-a"
		nsB    = "v2-ssname-held-b"
		acme   = "https://github.com/acme-held"
		shared = "shared-held"
	)
	bypassV2Admission(t)
	createNamespace(t, nsA)
	createNamespaceWithLabels(t, nsB, map[string]string{admissionBypassLabel: "true"})
	createGitHubAppSecret(t, nsA, "github-app")
	createGitHubAppSecret(t, nsB, "github-app")
	cleanup := func(obj client.Object) {
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)) })
	}
	startActionsGatewayV2Reconciler(t)
	for _, obj := range []client.Object{
		ghesAdmissionGateway("gw", nsA, acme, ""),
		newScaleSetRunnerSet("held", nsA, "gw", shared),
	} {
		require.NoError(t, k8sClient.Create(ctx, obj))
		cleanup(obj)
	}
	heldKey := types.NamespacedName{Namespace: nsA, Name: "gw"}
	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		return k8sClient.Get(ctx, heldKey, &got) == nil &&
			controllerutil.ContainsFinalizer(&got, v2.ActionsGatewayFinalizer)
	}, 30*time.Second, 100*time.Millisecond, "the held gateway must be reconciled first")

	for _, obj := range []client.Object{
		ghesAdmissionGateway("gw", nsB, acme, ""),
		newScaleSetRunnerSet("racer", nsB, "gw", shared),
	} {
		requireCreateEventually(t, obj)
		cleanup(obj)
	}
	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, heldKey, &got); err != nil {
			return false
		}
		c := findCondition(got.Status.Conditions, v2.ConditionScaleSetNameCollision)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2.ReasonScaleSetNameShared
	}, 30*time.Second, 100*time.Millisecond, "the held gateway must report the stored pair")

	// Retried only for an update conflict with the reconciler's own writes.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, heldKey, &got); err != nil {
			return err
		}
		got.Labels = map[string]string{"team": "a"}
		return k8sClient.Update(ctx, &got)
	})
	require.NoError(t, err, "the held tenant's edit to its own gateway must be admitted")
}

// TestV2_ScaleSetNameGuard_PresetFinalizerIsHeld covers Q1358: a raced gateway that
// arrives already carrying the GMC finalizer, created that way or restored by a backup
// tool that keeps finalizers, is still held unprovisioned. The pair is stored with
// admission bypassed, then the bypass is lifted before the reconciler starts, so the
// GMC's first update to the raced gateway goes through the webhook as a raced pair's
// would.
func TestV2_ScaleSetNameGuard_PresetFinalizerIsHeld(t *testing.T) {
	const (
		nsA    = "v2-ssname-preset-a"
		nsB    = "v2-ssname-preset-b"
		acme   = "https://github.com/acme-preset"
		shared = "shared-preset"
	)
	bypassV2Admission(t)
	createNamespace(t, nsA)
	createNamespaceWithLabels(t, nsB, map[string]string{admissionBypassLabel: "true"})
	createGitHubAppSecret(t, nsA, "github-app")
	createGitHubAppSecret(t, nsB, "github-app")
	cleanup := func(obj client.Object) {
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)) })
	}
	held := ghesAdmissionGateway("gw", nsA, acme, "")
	for _, obj := range []client.Object{held, newScaleSetRunnerSet("held", nsA, "gw", shared)} {
		require.NoError(t, k8sClient.Create(ctx, obj))
		cleanup(obj)
	}
	// The held tenant is established before the pair exists: the finalizer is added while
	// nothing collides, and the status the GMC would have written marks it reconciled.
	controllerutil.AddFinalizer(held, v2.ActionsGatewayFinalizer)
	require.NoError(t, k8sClient.Update(ctx, held))
	held.Status.ObservedGeneration = held.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, held))

	raced := ghesAdmissionGateway("gw", nsB, acme, "")
	controllerutil.AddFinalizer(raced, v2.ActionsGatewayFinalizer)
	for _, obj := range []client.Object{raced, newScaleSetRunnerSet("racer", nsB, "gw", shared)} {
		requireCreateEventually(t, obj)
		cleanup(obj)
	}

	// Lift the bypass and wait until the webhook judges nsB again: a second set on the
	// held name is refused only once it does.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ns corev1.Namespace
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: nsB}, &ns); err != nil {
			return err
		}
		delete(ns.Labels, admissionBypassLabel)
		return k8sClient.Update(ctx, &ns)
	}))
	require.Eventually(t, func() bool {
		probe := newScaleSetRunnerSet("probe", nsB, "gw", shared)
		if err := k8sClient.Create(ctx, probe); err == nil {
			_ = k8sClient.Delete(ctx, probe)
			return false
		}
		return true
	}, 30*time.Second, 200*time.Millisecond, "the webhook must judge %s again before the reconciler starts", nsB)

	startActionsGatewayV2Reconciler(t)
	heldKey := types.NamespacedName{Namespace: nsA, Name: "gw"}
	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, heldKey, &got); err != nil {
			return false
		}
		c := findCondition(got.Status.Conditions, v2.ConditionScaleSetNameCollision)
		return c != nil && c.Status == metav1.ConditionTrue
	}, 30*time.Second, 100*time.Millisecond, "the held gateway must report the stored pair")

	// Once past its handshake update, the GMC writes this fixture's status within the
	// same reconcile (the held gateway's report above), so a raced gateway it never
	// writes status for is one it never provisioned.
	racedKey := client.ObjectKeyFromObject(raced)
	require.Never(t, func() bool {
		var got v2.ActionsGateway
		return k8sClient.Get(ctx, racedKey, &got) == nil && got.Status.ObservedGeneration > 0
	}, 5*time.Second, 100*time.Millisecond, "the GMC must not reconcile past the handshake a raced gateway with a preset finalizer")
}
