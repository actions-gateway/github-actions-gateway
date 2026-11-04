//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q422 experiment 4 half A — the pre-claim quota rung (#784) under real contention,
// against a real apiserver rather than a fake client.
//
// What already existed: unit coverage of the arithmetic and of Admit's return values
// over a fake client (provisioner/worker_quota_internal_test.go), and envtest coverage
// of the *advisory* WorkerQuota{Pressure,Exceeded} conditions
// (v2_runnerset_capacity_test.go). Neither observes the thing the rung exists to do —
// leave the job at GitHub instead of claiming it. These tests close that gap on both
// tiers the rung now serves: the classic per-delivered-job form (Provisioner.Admit) and
// the scale-set per-poll form (Provisioner.AdvertiseCapacity, ported by Q443).
//
// Both fill the quota the way a busy namespace does — `status.used` at or near
// `status.hard` — rather than declaring a hard limit too small to ever fit a worker.
// That is what exercises the `hard − used` remaining arithmetic the gate actually runs;
// every existing envtest sets only `spec.hard`. envtest runs no resourcequota
// controller, so the tests own `status` outright and nothing rewrites it underneath
// them.
//
// The coverage these two cases provide, and what envtest cannot show, are recorded in
// docs/design/07-test-plan.md § Pre-claim quota gate under contention (Q422).

// filledCPUQuota builds a namespace ResourceQuota that is *occupied*: hard and used are
// both set on the status, so remaining headroom is hard − used. The gate prefers
// status.hard over spec.hard (QuotaHeadroomViolations), so the status is the operative
// half and spec.hard only mirrors it.
//
// Occupancy is adjusted afterwards with refillCPUQuota or freeCPUQuota.
func filledCPUQuota(t *testing.T, ns, name, hard, used string) {
	t.Helper()
	q := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, q))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), q) })
	refillCPUQuota(t, ns, name, hard, used)
}

// refillCPUQuota rewrites a ResourceQuota's status.hard/status.used, standing in for
// the resourcequota controller envtest does not run. Lowering `used` is how these tests
// model sibling jobs finishing and headroom returning.
func refillCPUQuota(t *testing.T, ns, name, hard, used string) {
	t.Helper()
	var live corev1.ResourceQuota
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &live))
	live.Status = corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)},
		Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(used)},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, &live))
}

// TestV2_RunnerSet_ScaleSet_PartialQuotaHeadroomSplitsTheCeiling is Q422 half A at the
// scale-set tier, which the rung reached in Q443 (#868) and whose footprint arithmetic
// Q450 (#883) corrected.
//
// TestV2_RunnerSet_ScaleSet_QuotaHeadroomBoundsAdvertisedCapacity already covers the
// all-or-nothing case: a quota too small for one worker withholds the entire ceiling
// and a queued job is never assigned. The gap this closes is the *partial* case under
// an occupied quota, which is where a double-count hides. AdvertiseCapacity converts a
// headroom delta into a total, so the invariant that must hold on every poll is
//
//	advertised + withheld == declared ceiling
//
// Counting the same slot on both sides of that split — or reserving one, as the classic
// rung deliberately does not — breaks it while the all-or-nothing case (0 + ceiling)
// still passes.
func TestV2_RunnerSet_ScaleSet_PartialQuotaHeadroomSplitsTheCeiling(t *testing.T) {
	const ns = "v2-rs-q422-partial-quota"
	const label = "linux-q422"
	const setName = "ss-q422"
	const ceiling = int32(4)
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, ceiling)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// 4 CPU granted, 3 already consumed: 1 CPU free admits exactly two more 500m
	// workers, half the set's declared ceiling of 4.
	filledCPUQuota(t, ns, "tenant", "4", "3")

	startRunnerSetReconcilerWithScaleSet(t, srv)

	require.Eventually(t, func() bool {
		_, ok := srv.ScaleSetIDByName(label)
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2.ReasonListenerActive)

	// Partial headroom lowers the advertisement to what fits and attributes the
	// remainder to the quota rung — the two summing to the untouched declared ceiling.
	require.Eventually(t, func() bool {
		advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
		withheld := testutil.ToFloat64(
			scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonQuota))
		return advertised == 2 && withheld == 2
	}, 20*time.Second, 100*time.Millisecond,
		"1 CPU of headroom must advertise the 2 workers that fit and withhold the other 2 of the ceiling")

	advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
	withheld := testutil.ToFloat64(
		scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonQuota))
	require.Equal(t, float64(ceiling), advertised+withheld,
		"the quota rung splits the declared ceiling, it does not consume from it")

	// Refilling the quota to zero headroom drops the advertisement to zero without the
	// declared ceiling moving — the same invariant at the other end of the range, and
	// proof the rung re-reads live quota per poll rather than latching (Q117).
	refillCPUQuota(t, ns, "tenant", "4", "4")

	require.Eventually(t, func() bool {
		a := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
		w := testutil.ToFloat64(
			scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonQuota))
		return a == 0 && w == float64(ceiling)
	}, 20*time.Second, 100*time.Millisecond,
		"a quota refilled to zero headroom must withhold the whole ceiling on the next poll")

	// No worker pod was ever built: a slot withheld here is a job GitHub never assigns,
	// so unlike the classic tier nothing is claimed and then unwound.
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{provisioner.LabelRunnerSet: setName}))
	require.Empty(t, pods.Items, "no worker pod should exist while the set advertises no capacity")
}
