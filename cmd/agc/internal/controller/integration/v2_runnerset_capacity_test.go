//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// Q303 worker-capacity conditions on the v2 RunnerSet, proven against a real
// apiserver with the manager's Pod watch and RequeueAfter loop running. These are
// the v2 port of the v1 RunnerGroup evaluations: without them a stalled RunnerSet
// shows only a rising status.pendingJobs with Ready=True, hiding the stall. envtest
// runs no scheduler and no resourcequota controller, so the tests play both roles —
// marking a pod Unschedulable, and declaring a ResourceQuota whose spec.hard the
// reconciler reads directly (status.hard/used stay empty without the controller).

// setCondition fetches a single status condition from the named RunnerSet.
func setCondition(t *testing.T, ns, name, condType string) *metav1.Condition {
	t.Helper()
	var rs v2alpha1.RunnerSet
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
		return nil
	}
	return meta.FindStatusCondition(rs.Status.Conditions, condType)
}

// createV2WorkerPod creates a minimal pod carrying the v2 runner-set label for setName.
func createV2WorkerPod(t *testing.T, ns, setName, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{provisioner.LabelRunnerSet: setName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
	return pod
}

// newRunnerTemplateWithCPURequest is newRunnerTemplate with a per-worker CPU request,
// so the worker-quota footprint has a non-zero requests.cpu to compare against a
// namespace ResourceQuota.
func newRunnerTemplateWithCPURequest(name, ns, cpu string) *v2alpha1.RunnerTemplate {
	tmpl := newRunnerTemplate(name, ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
	}
	return tmpl
}

// TestV2_RunnerSet_WorkersUnschedulable_Tripped proves a worker pod the scheduler
// cannot place (non-quota) trips WorkersUnschedulable on the RunnerSet once it ages
// past the grace (half pendingPodDeadline), mirroring the v1 RunnerGroup behaviour
// (Q157 → Q303).
func TestV2_RunnerSet_WorkersUnschedulable_Tripped(t *testing.T) {
	const ns = "v2-rs-unsched"
	createNSForAGC(t, ns)

	// Direct egress (no proxy) keeps setup minimal; the set still reaches ListenerActive.
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("unsched-set", ns, "gw")
	// A 12s pending deadline gives a 6s scheduling grace and a 6s window before the
	// reaper deletes the pod at 12s.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 12 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	// Wait until references resolve and a listener is active, so the capacity-condition
	// code path is reached each reconcile.
	waitForSetReadyReason(t, ns, "unsched-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	pod := createV2WorkerPod(t, ns, "unsched-set", "worker-unsched")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")

	// Within the grace the condition must be False (or not yet set).
	if c := setCondition(t, ns, "unsched-set", v2alpha1.ConditionWorkersUnschedulable); c != nil {
		require.Equal(t, metav1.ConditionFalse, c.Status, "must not trip before the grace elapses")
	}

	// After the grace the reconciler (driven by its own RequeueAfter) must flip the
	// condition True and name the stuck pod, before the reaper removes it at 12s.
	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "unsched-set", v2alpha1.ConditionWorkersUnschedulable)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2alpha1.ReasonPodsUnschedulable
	}, 11*time.Second, 100*time.Millisecond,
		"a scheduler-unschedulable worker pod must trip WorkersUnschedulable on the RunnerSet")

	c := setCondition(t, ns, "unsched-set", v2alpha1.ConditionWorkersUnschedulable)
	require.Contains(t, c.Message, "worker-unsched")
	require.Contains(t, c.Message, "untolerated taint")
}

// TestV2_RunnerSet_WorkerQuotaExceeded_Tripped proves that a namespace ResourceQuota
// with no headroom for even one worker pod trips WorkerQuotaExceeded on the RunnerSet
// (Q82 → Q303). The condition is advisory and never gates Ready — the set stays
// Ready=True while surfacing the capacity error that a rising pendingJobs would hide.
func TestV2_RunnerSet_WorkerQuotaExceeded_Tripped(t *testing.T) {
	const ns = "v2-rs-quota-exceeded"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newRunnerSet("quota-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))

	// Quota headroom below a single worker's 500m request → the next worker pod would
	// be rejected at admission.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "quota-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "quota-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "QuotaExhausted"
	}, 20*time.Second, 100*time.Millisecond,
		"an exhausted namespace ResourceQuota must trip WorkerQuotaExceeded on the RunnerSet")

	// Ready must remain True — the quota condition is advisory, not a Ready gate.
	ready := setCondition(t, ns, "quota-set", v2alpha1.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status, "WorkerQuotaExceeded must not gate Ready")
}

// TestV2_RunnerSet_WorkerQuotaPressure_Tripped proves the warning tier: quota headroom
// admits one worker but not the pool's full ceiling (maxWorkers), so the set reports
// WorkerQuotaPressure (not Exceeded) — the two are mutually exclusive (Q82 → Q303).
func TestV2_RunnerSet_WorkerQuotaPressure_Tripped(t *testing.T) {
	const ns = "v2-rs-quota-pressure"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newRunnerSet("pressure-set", ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3)) // ceiling 3 → footprint 1500m
	require.NoError(t, k8sClient.Create(ctx, rs))

	// 1000m admits one 500m worker (not Exceeded) but not the full 1500m ceiling (Pressure).
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "roomy", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	waitForSetReadyReason(t, ns, "pressure-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool {
		p := setCondition(t, ns, "pressure-set", v2alpha1.ConditionWorkerQuotaPressure)
		e := setCondition(t, ns, "pressure-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return p != nil && p.Status == metav1.ConditionTrue && p.Reason == "InsufficientQuotaHeadroom" &&
			e != nil && e.Status == metav1.ConditionFalse
	}, 20*time.Second, 100*time.Millisecond,
		"a quota that admits one worker but not the ceiling must trip WorkerQuotaPressure, not Exceeded")
}

// TestV2_RunnerSet_QuotaEditRetriggersReconcile proves the RunnerSet reconciler's
// ResourceQuota watch (Q326) against real apiserver watch semantics: once the set is
// Ready with its listeners at the desired count, no timer requeues it — so a quota
// created out-of-band tripping WorkerQuotaExceeded, and a raised .spec.hard clearing
// it, can only have arrived through the watch + hard-changed predicate.
func TestV2_RunnerSet_QuotaEditRetriggersReconcile(t *testing.T) {
	const ns = "v2-rs-quota-watch"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newRunnerSet("watch-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	// ListenerActive means active listeners reached maxListeners: the baseline
	// recheck timer no longer arms, so the set goes dormant between watch events.
	waitForSetReadyReason(t, ns, "watch-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "watch-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionFalse
	}, 20*time.Second, 100*time.Millisecond, "steady state before the quota exists: Exceeded=False")

	// Headroom below one 500m worker: the quota's creation alone must re-reconcile
	// the set and trip the error tier.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), quota) })

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "watch-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "QuotaExhausted"
	}, 10*time.Second, 100*time.Millisecond,
		"creating a tight ResourceQuota must promptly trip WorkerQuotaExceeded via the quota watch (Q326)")

	// Raising .spec.hard passes the hard-changed predicate and must clear it.
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tight"}, quota))
	quota.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("100")
	require.NoError(t, k8sClient.Update(ctx, quota))

	require.Eventually(t, func() bool {
		c := setCondition(t, ns, "watch-set", v2alpha1.ConditionWorkerQuotaExceeded)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "NoRejection"
	}, 10*time.Second, 100*time.Millisecond,
		"raising the quota's .spec.hard must promptly clear WorkerQuotaExceeded via the watch predicate (Q326)")
}
