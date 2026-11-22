//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_ScaleSet_ReissueLoopIsSurfaced is Q1361. GitHub withdraws a
// scale-set assignment no runner takes within five minutes and re-issues the job under
// a new job ID. A worker bound to a node that accepts pods and cannot run them never
// starts, so the withdrawal stamps it, the reaper collects it 30 seconds later as
// completed_pending, and the re-issue's new worker sticks the same way: a fresh worker,
// and a fresh runner registration, every five minutes until GitHub gives up.
//
// envtest runs no kubelet, so a bound pod here is genuinely one that never starts. The
// test reproduces two turns of the loop and requires each to be named on the RunnerSet
// as a WorkerReplacedBeforeStart naming the node, rather than as the
// WorkerPodCompletedPending a cancellation produces.
func TestV2_RunnerSet_ScaleSet_ReissueLoopIsSurfaced(t *testing.T) {
	const ns = "v2-rs-ss-reissue"
	const label = "linux-reissue"
	const setName = "ss-reissue"
	const node = "node-cannot-run"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, 3)
	// Only the withdrawal's completion stamp may reap a worker here.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	recorder := events.NewFakeRecorder(64)
	startRunnerSetReconcilerWithScaleSetClients(t, scaleSetClientsFor(srv), func(r *controller.RunnerSetReconciler) {
		r.Recorder = recorder
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2.ReasonListenerActive)

	_, jobID := srv.EnqueueJob(ssID)
	worker := waitForSoleWorkerPod(t, ns, setName)

	for turn := 1; turn <= 2; turn++ {
		// The scheduler's part: the pod lands on a node that will never start it.
		require.NoError(t, k8sClient.SubResource("binding").Create(ctx, worker,
			&corev1.Binding{Target: corev1.ObjectReference{Kind: "Node", Name: node}}))

		// GitHub's part. CreationTimestamp is whole seconds, and the replacement must
		// read as created after the worker it replaces, as five minutes guarantees live.
		time.Sleep(1100 * time.Millisecond)
		jobID = srv.ReissueJob(ssID, jobID, "canceled")
		require.NotEmpty(t, jobID, "turn %d: the worker's job must be assigned to re-issue it", turn)

		replacement := waitForWorkerBeside(t, ns, setName, worker.Name)
		// The withdrawal stamps the stuck worker, and the reaper collects it once the 30s
		// grace runs out. A back-dated stamp would not hurry it: a metadata patch starts
		// no reconcile, so the reap waits on the requeue the stamp scheduled anyway. A
		// bound pod stays Terminating with no kubelet to confirm the delete.
		waitForStamp(t, ns, worker.Name)
		require.Eventually(t, func() bool { return podGoneOrTerminating(ns, worker.Name) },
			45*time.Second, 200*time.Millisecond, "turn %d: the withdrawn worker must be reaped", turn)

		require.Eventually(t, func() bool {
			for {
				select {
				case e := <-recorder.Events:
					if strings.Contains(e, "WorkerReplacedBeforeStart") &&
						strings.Contains(e, worker.Name) && strings.Contains(e, replacement.Name) &&
						strings.Contains(e, node) {
						return true
					}
				default:
					return false
				}
			}
		}, 10*time.Second, 100*time.Millisecond,
			"turn %d: the RunnerSet must name the worker that never started, its node and its replacement", turn)

		worker = replacement
	}
}

// waitForWorkerBeside waits for a worker pod of setName other than existing.
func waitForWorkerBeside(t *testing.T, ns, setName, existing string) *corev1.Pod {
	t.Helper()
	var found corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: setName}); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if p.Name != existing && p.DeletionTimestamp.IsZero() {
				found = p
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "the re-issued job must get a worker of its own")
	return &found
}

// podGoneOrTerminating reports whether name is deleted or carries a deletion timestamp.
func podGoneOrTerminating(ns, name string) bool {
	var p corev1.Pod
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)
	return apierrors.IsNotFound(err) || (err == nil && !p.DeletionTimestamp.IsZero())
}

// waitForStamp waits for the listener's completion stamp on name.
func waitForStamp(t *testing.T, ns, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var p corev1.Pod
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			return false
		}
		_, ok := p.Annotations[provisioner.AnnotationJobCompletedAt]
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the withdrawal must stamp the worker that never started")
}
