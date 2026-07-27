//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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

// freeCPUQuota releases all of a ResourceQuota's headroom, modelling sibling jobs
// finishing. The status write lands FIRST and the spec.hard raise second, deliberately:
// only a spec.hard change passes the reconcilers' quota watch predicate
// (quotaHardChangedPredicate ignores the high-frequency status.used churn), so writing
// it last guarantees the reconcile it triggers already sees the freed status. That is
// what makes the RunnerGroup's WorkerQuotaExceeded condition usable as a cache-freshness
// signal — it flips only once the manager's informer holds the new quota, which is the
// same read the admission gate does per delivered job.
func freeCPUQuota(t *testing.T, ns, name, hard string) {
	t.Helper()
	refillCPUQuota(t, ns, name, hard, "0")

	var live corev1.ResourceQuota
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &live))
	live.Spec.Hard = corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hard)}
	require.NoError(t, k8sClient.Update(ctx, &live))
}

// waitForRGQuotaExceeded blocks until the RunnerGroup's WorkerQuotaExceeded condition
// reaches want. The reconciler computes it from the manager's cached quota list — the
// same read Provisioner.Admit makes — so it is the observable for "the gate now sees
// this quota".
func waitForRGQuotaExceeded(t *testing.T, ns, name string, want metav1.ConditionStatus) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rg v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rg); err != nil {
			return false
		}
		c := apimeta.FindStatusCondition(rg.Status.Conditions, v1alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == want
	}, 30*time.Second, 100*time.Millisecond,
		"the RunnerGroup's WorkerQuotaExceeded condition should reach %s", want)
}

// admissionRejections reads the current value of
// actions_gateway_jobs_admission_rejected_total for one (namespace, group, reason)
// series. The counters are process-wide (sharedListenerMetrics), so every assertion
// here is a delta against a baseline read before the work starts.
func admissionRejections(ns, group, reason string) float64 {
	return testutil.ToFloat64(
		sharedListenerMetrics().JobsAdmissionRejectedTotal.WithLabelValues(ns, group, reason))
}

// TestAGC_AdmissionGate_QuotaRungSkipsAcquireUnderContention is Q422 half A on the
// classic tier: with the namespace ResourceQuota full, jobs delivered to a live
// listener must be left queued at GitHub rather than claimed.
//
// Three assertions, in the plan's order:
//
//	(a) acquirejob is SKIPPED — asserted server-side on the broker stub's call
//	    counter, not by the absence of a worker pod. "No pod appeared" would also
//	    pass if the AGC had claimed the job and then failed to place it, which is
//	    precisely the claim-and-stall this rung exists to prevent.
//	(b) actions_gateway_jobs_admission_rejected_total{reason="quota"} increments once
//	    per skipped delivery, so an operator can tell quota pressure from a ceiling.
//	(c) the ceiling budget is UNTOUCHED. maxWorkers is 1, so a single leaked
//	    reservation would permanently close the gate; after headroom returns, a job
//	    is acquired and a worker pod is built, and the reason="ceiling" series never
//	    moves. A quota rung that double-counted into the in-memory reservation gate
//	    could not produce that outcome.
func TestAGC_AdmissionGate_QuotaRungSkipsAcquireUnderContention(t *testing.T) {
	const nsName = "agc-q422-quota-gate"
	const rgName = "quota-gate-rg"
	createNSForAGC(t, nsName)

	// maxWorkers=1 makes the ceiling budget observable: with one slot, any reservation
	// leaked by a quota refusal is fatal to the recovery step below.
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: rgName, Namespace: nsName},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxListeners: 2,
			MaxWorkers:   ptr.To(int32(1)),
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: "runner:test",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
						},
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// The tenant is sitting exactly at its ceiling: 2 CPU granted, 2 CPU consumed by
	// work already running. Zero headroom for the next 500m worker.
	filledCPUQuota(t, nsName, "tenant", "2", "2")

	startAGCReconcilerWithProvisioner(t, provisionerOptions{metrics: sharedListenerMetrics()})

	acquiresBefore := brokerStub.AcquireJobCalls()
	quotaBefore := admissionRejections(nsName, rgName, runnercore.AdmitReasonQuota)
	ceilingBefore := admissionRejections(nsName, rgName, runnercore.AdmitReasonCeiling)

	// Submit more work than fits — three jobs against a quota that cannot admit one.
	// Enqueued one at a time on whichever session is live, so each delivery is
	// observed before the next is submitted; a rejected delivery does not consume the
	// JIT agent, so the session stays available for the next.
	const overSubmitted = 3
	for i := 1; i <= overSubmitted; i++ {
		id := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{})
		require.NotEmpty(t, id, "a session for %s should be active for delivery %d", rgName, i)

		require.Eventually(t, func() bool {
			return admissionRejections(nsName, rgName, runnercore.AdmitReasonQuota) >= quotaBefore+float64(i)
		}, 20*time.Second, 25*time.Millisecond,
			"delivery %d must be rejected with reason=quota", i)
	}

	// (a) Not one of them was claimed. The counter is monotonic and only this test
	// enqueues while it runs, so a flat count is a real skip.
	require.Never(t, func() bool {
		return brokerStub.AcquireJobCalls() > acquiresBefore
	}, 2*time.Second, 100*time.Millisecond,
		"a quota-blocked job must be left queued at GitHub, never claimed by acquirejob")

	// (b) Exactly the quota series moved.
	require.Equal(t, float64(overSubmitted),
		admissionRejections(nsName, rgName, runnercore.AdmitReasonQuota)-quotaBefore,
		"each skipped delivery must increment the quota series exactly once")
	require.Equal(t, ceilingBefore, admissionRejections(nsName, rgName, runnercore.AdmitReasonCeiling),
		"a quota refusal must not be attributed to the configured ceiling")

	// Nothing was provisioned and no job Secret was staged: the gate ran before any
	// of the provisioning path, not as a rollback partway through it.
	require.Zero(t, countWorkerPods(t, nsName, rgName), "no worker pod may exist for a job never claimed")
	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &secrets,
		client.InNamespace(nsName), client.MatchingLabels{"actions-gateway/runner-group": rgName}))
	for _, s := range secrets.Items {
		require.False(t, strings.HasPrefix(s.Name, "job-"),
			"no per-job Secret may be staged for a job never claimed (found %q)", s.Name)
	}

	// (c) Headroom returns — sibling jobs finished and freed the whole 2 CPU. The gate
	// is self-clearing: no AGC restart, no reset of the reservation counter.
	//
	// Wait for the condition before enqueuing so exactly ONE more job is submitted.
	// That precision is load-bearing for the ceiling assertion below: a retry loop that
	// enqueued several deliveries could get one admitted and the next legitimately
	// refused at maxWorkers=1, muddying the very series being asserted on.
	freeCPUQuota(t, nsName, "tenant", "2")
	waitForRGQuotaExceeded(t, nsName, rgName, metav1.ConditionFalse)

	id := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for %s should still be active", rgName)

	require.Eventually(t, func() bool {
		return brokerStub.AcquireJobCalls() > acquiresBefore
	}, 20*time.Second, 25*time.Millisecond,
		"freed quota headroom must reopen the gate and let the next delivery be claimed")

	waitForWorkerPod(t, nsName, rgName)

	// The heart of (c): maxWorkers is 1. Had any of the quota refusals reserved a
	// ceiling slot, the reservation gate would still be holding it — the acquire above
	// could not have happened, and the attempt would have been counted under
	// reason=ceiling instead. Both facts together are what a double-count breaks.
	require.Equal(t, ceilingBefore, admissionRejections(nsName, rgName, runnercore.AdmitReasonCeiling),
		"the quota rung reserves nothing: the ceiling budget must be untouched by %d quota refusals", overSubmitted)
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
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// 4 CPU granted, 3 already consumed: 1 CPU free admits exactly two more 500m
	// workers, half the set's declared ceiling of 4.
	filledCPUQuota(t, ns, "tenant", "4", "3")

	startRunnerSetReconcilerWithScaleSet(t, srv)

	require.Eventually(t, func() bool {
		_, ok := srv.ScaleSetIDByName(label)
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

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
