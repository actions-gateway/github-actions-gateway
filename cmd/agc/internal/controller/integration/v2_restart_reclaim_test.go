//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q435: the reaper is documented as restart-safe — it lives in the reconciler
// rather than the session goroutine precisely so it "also reaps pods orphaned by
// an AGC crash" (02-architecture.md). That claim had never been measured against
// the state a crashed AGC actually leaves behind: worker pods that already exist
// when the process starts, for which no goroutine, listener, or session state
// survives.
//
// The restart is modelled by creating the orphans BEFORE starting the manager.
// That is the whole of what an AGC restart means to the reaper: the pods are in
// the apiserver, and the new process has no in-memory knowledge of any of them.
// A test that starts the reconciler first would instead be measuring the live
// path, which the Q95/Q420 suites already cover.
//
// The four classes below are exhaustive over what the reaper can see on a worker
// pod it did not create — phase, plus the presence of the completion stamp.

// createOrphanedV2WorkerPod creates a worker pod for setName as if a previous AGC
// process had provisioned it and then died. The pod is left in the phase and with
// the annotations the caller asks for, and no reconciler is running yet.
func createOrphanedV2WorkerPod(t *testing.T, ns, setName, name string, mutate func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	pod := createV2WorkerPod(t, ns, setName, name)
	if mutate != nil {
		mutate(pod)
	}
	return pod
}

// stampCompletedAgo back-dates the scale-set completion annotation on pod as if
// GitHub had declared its job terminal completedAgo in the past.
func stampCompletedAgo(t *testing.T, pod *corev1.Pod, completedAgo time.Duration) {
	t.Helper()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[provisioner.AnnotationJobCompletedAt] =
		time.Now().Add(-completedAgo).UTC().Format(time.RFC3339)
	require.NoError(t, k8sClient.Update(ctx, pod))
}

// driveToRunning flips pod to Running through the status subresource, the role the
// kubelet plays in a real cluster (envtest runs none).
func driveToRunning(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// driveToSucceeded flips pod to Succeeded with a terminated container status, so
// podTerminalTime reads finishedAt rather than falling back to creationTimestamp.
func driveToSucceeded(t *testing.T, pod *corev1.Pod, finishedAgo time.Duration) {
	t.Helper()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: provisioner.WorkerContainerName,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   0,
				FinishedAt: metav1.NewTime(time.Now().Add(-finishedAgo)),
			},
		},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestV2_RunnerSet_RestartReclaimsOrphanedWorkers measures which worker pods a
// freshly started AGC reclaims when they were orphaned before it came up — the
// shape of the dogfood incident behind Q435, where the AGC was evicted with jobs
// in flight and its workers pinned their nodes for 16 hours.
//
// Three of the four classes are reclaimed, because their deadline is derivable
// from state the pod itself carries. The fourth is not: a Running pod with no
// completion stamp has no deadline of any kind, and the only writer of that stamp
// is a live listener processing the job's terminal message. An AGC that was down
// when the job ended never wrote it, and nothing on restart backfills it — so the
// pod is retained forever. That is asserted here as the measured behaviour, not
// as the desired one: the durable-deadline fix for it is tracked as Q438, and the
// operator-facing recovery is in operations/troubleshooting.md ("Workers Left
// Behind by an AGC That Was Down").
func TestV2_RunnerSet_RestartReclaimsOrphanedWorkers(t *testing.T) {
	const ns = "v2-rs-restart-reclaim"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("restart-set", ns, "gw")
	// Short, so the phase-based arms come due promptly; both are well inside the
	// Eventually budget below.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: 2 * time.Second}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 3 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns},
		})
	})

	// --- The previous AGC process's leftovers, staged before anything is running.

	// 1. Terminal well past completedPodTTL: the runner ran its job and exited,
	//    and the AGC died before the routine cleanup fired.
	createOrphanedV2WorkerPod(t, ns, "restart-set", "orphan-terminal", func(p *corev1.Pod) {
		driveToSucceeded(t, p, time.Hour)
	})

	// 2. Pending past pendingPodDeadline: the pod never scheduled (envtest runs no
	//    scheduler, so this is genuinely stuck, not simulated).
	createOrphanedV2WorkerPod(t, ns, "restart-set", "orphan-pending", nil)

	// 3. Running, stamped before the crash: the previous process saw the job go
	//    terminal at GitHub and wrote the annotation, then died before reaping.
	createOrphanedV2WorkerPod(t, ns, "restart-set", "orphan-running-stamped", func(p *corev1.Pod) {
		stampCompletedAgo(t, p, time.Hour)
		driveToRunning(t, p)
	})

	// 4. Running, never stamped: the AGC was already down when the job ended, so
	//    nobody wrote the stamp. This is the dogfood incident's shape.
	createOrphanedV2WorkerPod(t, ns, "restart-set", "orphan-running-unstamped", func(p *corev1.Pod) {
		driveToRunning(t, p)
	})

	// --- The restart: a brand-new manager meets pods it never provisioned.

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "restart-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	for _, name := range []string{"orphan-terminal", "orphan-pending", "orphan-running-stamped"} {
		require.Eventually(t, func() bool { return podGone(ns, name) },
			30*time.Second, 100*time.Millisecond,
			"%s carries its own reap deadline, so a restarted AGC must reclaim it", name)
	}

	// The measured gap. The reaper is active — it has just deleted three pods —
	// yet this one has no deadline to come due, so it is retained indefinitely.
	require.Never(t, func() bool { return podGone(ns, "orphan-running-unstamped") },
		3*time.Second, 200*time.Millisecond,
		"measured: a Running worker orphaned with no completion stamp is never reclaimed")

	// And it stays unstamped: nothing in the reconcile path backfills the stamp
	// from cluster state alone. The one thing that can is a replayed completion
	// from GitHub — measured separately below.
	var orphan corev1.Pod
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Namespace: ns, Name: "orphan-running-unstamped"}, &orphan))
	require.NotContains(t, orphan.Annotations, provisioner.AnnotationJobCompletedAt,
		"no reconcile-path writer backfills the completion stamp for a job the process never saw")
}

// TestV2_RunnerSet_ScaleSet_RestartReclaimsWorkerOrphanedWhileDown measures the one
// recovery path a restarted AGC does have for the gap above, as a real restart: one
// manager provisions a worker and is then shut down, the job goes terminal at GitHub
// while nothing is running, and a second manager comes up to the leftover pod.
//
// The mechanism under test is that completeJob runs its reclaim hook on every
// delivery rather than only on jobs the current process acquired, so a JobCompleted
// still in the scale set's queue when the AGC returns is enough to stamp the pod —
// after which the ordinary five-minute grace applies and
// TestV2_RunnerSet_OrphanedRunningPodReaped covers the reap itself. The pod is named
// by the provisioner, not by the test, so nothing here can drift from the real
// naming.
func TestV2_RunnerSet_ScaleSet_RestartReclaimsWorkerOrphanedWhileDown(t *testing.T) {
	const ns = "v2-rs-ss-restart"
	const label = "linux-ss-restart"
	const setName = "ss-restart"

	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, 3)
	// Keep every phase-based arm out of the way: only the completion stamp may give
	// this pod a deadline, so a pass cannot be another arm in disguise.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// --- The AGC process that provisions the worker and then dies.

	stopFirst := startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	_, jobID := srv.EnqueueJob(ssID)

	orphan := waitForSoleWorkerPod(t, ns, setName)
	// The kubelet's part: the runner came up and is executing its job.
	driveToRunning(t, orphan)
	require.NotContains(t, orphan.Annotations, provisioner.AnnotationJobCompletedAt,
		"a worker whose job is still assigned must not carry a completion stamp")

	stopFirst()

	// Q222's contract, asserted where it is decided rather than 30s downstream: a
	// drained AGC leaves no session behind, because the scale set admits exactly one
	// and the successor cannot open its own until this is gone. Checked here because
	// the failure it catches is otherwise indistinguishable from a slow completion —
	// the stub expires nothing, so a session still held at this line means the wait
	// at the end of this test cannot ever be satisfied, and it burns its whole budget
	// before saying so (Q968).
	require.False(t, srv.HasActiveSession(ssID),
		"a drained AGC must leave no scale-set session behind; one held here locks the successor out permanently")

	// --- The job goes terminal at GitHub with no AGC listening. This is the window
	// the dogfood incident sat in for 16 hours: nobody is left to stamp the pod.

	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "succeeded"),
		"the job must be assigned server-side before it can complete")

	// --- The restart: a new process, a new session, and a pod it never created.

	startRunnerSetReconcilerWithScaleSet(t, srv)

	// The wait is against live state, so it keeps its subject's output: a bare
	// "Condition never satisfied" cannot say whether the completion was never
	// delivered or the restarted listener never got a session to receive it on, and
	// on a flake the run that could have answered is gone (Q968).
	if !assert.Eventually(t, func() bool {
		var got corev1.Pod
		if err := k8sClient.Get(ctx,
			types.NamespacedName{Namespace: ns, Name: orphan.Name}, &got); err != nil {
			return false
		}
		_, ok := got.Annotations[provisioner.AnnotationJobCompletedAt]
		return ok
	}, 30*time.Second, 100*time.Millisecond,
		"a completion delivered to the restarted AGC's session must stamp the orphaned worker, giving it a reap deadline") {
		t.Logf("scale set %d still holds a session: %v", ssID, srv.HasActiveSession(ssID))
		t.Logf("session calls in order: %v", sessionCalls(srv))
		if c := readySetCondition(t, ns, setName); c != nil {
			t.Logf("RunnerSet Ready=%s/%s: %s", c.Status, c.Reason, c.Message)
		}
		t.FailNow()
	}
}

// sessionCalls filters the stub's call log to the session lifecycle, which is what
// separates "the predecessor never deleted its session" from "the predecessor
// created a fresh one on its way out". Order is the whole signal, so it is kept.
func sessionCalls(srv *scalesettest.Server) []string {
	var out []string
	for _, c := range srv.Calls() {
		if strings.Contains(c, "-session ") {
			out = append(out, c)
		}
	}
	return out
}

// waitForSoleWorkerPod waits until setName has exactly one worker pod and returns it.
func waitForSoleWorkerPod(t *testing.T, ns, setName string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: setName}); err != nil {
			return false
		}
		if len(pods.Items) != 1 {
			return false
		}
		pod = pods.Items[0]
		return true
	}, 30*time.Second, 100*time.Millisecond, "the scale-set listener must provision exactly one worker pod")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), &pod) })
	return &pod
}
