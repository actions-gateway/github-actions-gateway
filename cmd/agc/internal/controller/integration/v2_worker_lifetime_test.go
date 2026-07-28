//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q438 closes the residual TestV2_RunnerSet_RestartReclaimsOrphanedWorkers
// measured and deliberately left open: a Running worker with no completion stamp
// has no deadline of any kind, so a restarted AGC retains it forever. The fix is a
// deadline stamped when the pod is created — the pod's activeDeadlineSeconds —
// because at reconcile time that orphan is indistinguishable from a worker running
// a legitimately long job.
//
// The mechanism is deliberately NOT the reaper: in the dogfood incident the AGC
// was Pending for the whole 16 hours, so a reaper-side deadline would not have
// bounded it either. activeDeadlineSeconds is enforced by the kubelet, which is
// the only actor still running in that failure.
//
// envtest runs no kubelet, so these tests split the claim in two and measure each
// half against a real apiserver: the deadline really is stamped on a pod the real
// provisioner created (below), and a pod killed the way a kubelet kills one is
// reclaimed legibly (further below). Neither half is asserted by inspection.

// driveToDeadlineExceeded flips pod to the terminal state a kubelet leaves behind
// when it kills a pod for exceeding activeDeadlineSeconds. This is the one part of
// the mechanism envtest cannot run for real; everything downstream of it — the
// reaper's classification, the metric, the Event — is the code under test.
func driveToDeadlineExceeded(t *testing.T, pod *corev1.Pod, finishedAgo time.Duration) {
	t.Helper()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "DeadlineExceeded"
	pod.Status.Message = "Pod was active on the node longer than the specified deadline"
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: provisioner.WorkerContainerName,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   137,
				Reason:     "DeadlineExceeded",
				FinishedAt: metav1.NewTime(time.Now().Add(-finishedAgo)),
			},
		},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestV2_RunnerSet_ScaleSet_WorkerCarriesLifetimeDeadline is the provision-time
// half: a worker pod created by the real provisioner, through the real scale-set
// path, carries activeDeadlineSeconds — and the apiserver accepts it.
//
// This matters more than a unit assertion on buildPod because activeDeadlineSeconds
// is validated server-side (it must be a positive integer). A defaulting bug that
// produced 0 or a negative would not fail a struct comparison in a fake client; it
// would fail every worker pod create in production. Here a real apiserver admits
// the pod or the test fails.
func TestV2_RunnerSet_ScaleSet_WorkerCarriesLifetimeDeadline(t *testing.T) {
	const ns = "v2-rs-ss-lifetime"
	const label = "linux-ss-lifetime"
	const setName = "ss-lifetime"

	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, 3)
	// A distinctive, non-default value, so a pass cannot come from the default
	// leaking through some other path.
	rs.Spec.MaxWorkerLifetime = &metav1.Duration{Duration: 7 * time.Hour}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	pod := waitForSoleWorkerPod(t, ns, setName)

	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds,
		"a provisioned worker must carry the lifetime cap the kubelet enforces (Q438)")
	require.Equal(t, int64(7*60*60), *pod.Spec.ActiveDeadlineSeconds,
		"the RunnerSet's maxWorkerLifetime must reach the pod verbatim")
}

// TestV2_RunnerSet_RestartReclaimsWorkerKilledByLifetimeCap is the Q438 residual
// class, now bounded. It is deliberately the same restart shape as
// TestV2_RunnerSet_RestartReclaimsOrphanedWorkers' fourth orphan — a worker the
// running process never knew about, with no completion stamp — with one thing
// changed: the kubelet has since killed it for exceeding its deadline.
//
// The Q435 test asserts that pod is retained forever with require.Never. This one
// asserts the same pod, once its deadline has fired, is reclaimed. Together they
// are the before and after of the gap.
func TestV2_RunnerSet_RestartReclaimsWorkerKilledByLifetimeCap(t *testing.T) {
	const ns = "v2-rs-lifetime-reclaim"

	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("lifetime-set", ns, "gw")
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: 2 * time.Second}
	// Long, so a pass cannot be the stuck-Pending arm firing on a pod that never
	// scheduled — envtest runs no scheduler.
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// The previous process's leftover, staged before anything runs. Unstamped and
	// Running is the dogfood incident's shape; the AGC was down when its job ended.
	killed := createOrphanedV2WorkerPod(t, ns, "lifetime-set", "orphan-deadline", func(p *corev1.Pod) {
		driveToRunning(t, p)
	})
	// A control that stays Running and unstamped: the deadline has not fired for
	// it, so the Q435 behaviour must be unchanged and it must survive. Without
	// this, a reaper bug that deleted every unstamped Running pod would pass.
	survivor := createOrphanedV2WorkerPod(t, ns, "lifetime-set", "orphan-still-running", func(p *corev1.Pod) {
		driveToRunning(t, p)
	})

	// The kubelet's part, which envtest cannot run: the deadline fires.
	driveToDeadlineExceeded(t, killed, time.Hour)

	// --- The restart: a brand-new manager meets a pod it never provisioned.

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "lifetime-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.Eventually(t, func() bool { return podGone(ns, "orphan-deadline") },
		30*time.Second, 100*time.Millisecond,
		"a worker killed by its lifetime cap carries a durable deadline, so a restarted AGC must reclaim it (Q438)")

	// The reaper is demonstrably live — it has just deleted the pod above — and the
	// unstamped Running worker is still retained. Q438 bounds the orphan; it does
	// not start reaping live jobs.
	require.Never(t, func() bool { return podGone(ns, "orphan-still-running") },
		3*time.Second, 200*time.Millisecond,
		"a Running worker whose deadline has not fired must still never be reaped")

	var got corev1.Pod
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Namespace: ns, Name: survivor.Name}, &got))
	require.Equal(t, corev1.PodRunning, got.Status.Phase)
}
