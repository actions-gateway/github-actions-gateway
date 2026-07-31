//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q421/Q502 — what does a node drain reach on each tier?
//
// `kubectl drain` does not signal pods; it POSTs to each pod's `pods/<name>/eviction`
// subresource, and a permitted eviction is a DELETE. That is a different thing from the
// kubelet's node-pressure eviction, which leaves a pod in PodFailed with
// Status.Reason == "Evicted". A real drain of a *running* worker ends in the kubelet
// winning the race against the object's removal: the pod publishes PodFailed with an
// empty reason while carrying its deletionTimestamp and a container exit the mark
// predates (measured at live GitHub, Q459). Since Q502 that shape IS recovered on both
// tiers — gated on the mark ordered against the recorded exit. A deleted worker with
// no container exit on record never ran its job to a reportable end and is
// deliberately not recovered, whether it vanishes outright or (as a real kubelet does
// even for a drained Pending pod — the fake-GitHub E2E_AGC_WorkerNodeDrain caught a
// mark-only rule firing on exactly that) publishes a transient Failed-with-mark first.
//
// These tests pin both sides of that boundary against a REAL apiserver, with worker
// pods that came out of the real provisioning path. envtest runs no kubelet, so an
// admitted eviction ordinarily removes the pod at once with no terminal phase — the
// unrecovered side. The recovered side is reproduced by holding the pod with a
// finalizer while its terminal status is written, which is exactly the object sequence
// a real kubelet produces (mark first, then the terminal phase with the exit record). The design boundary is
// docs/design/04-operational-flows.md §4.2; the operator-facing behaviour is
// docs/operations/troubleshooting.md, "Draining a Worker Auto-Re-Runs the Jobs It
// Interrupts".

// evictPod POSTs to the pod's eviction subresource, which is precisely what
// `kubectl drain` does for each pod it removes. It deliberately does NOT delete the pod
// directly: the whole question is whether the API path a drain uses reaches eviction
// recovery, and a plain Delete would beg it.
func evictPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	err := k8sClient.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
	})
	require.NoError(t, err, "the eviction subresource must admit the worker pod (no PDB guards worker pods)")
}

// requirePodGone blocks until the pod is absent from the API server.
func requirePodGone(t *testing.T, namespace, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var got corev1.Pod
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &got)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 50*time.Millisecond, "an admitted eviction must remove the worker pod")
}

// rerunCounter is a stand-in for the GitHub REST API that counts rerun-failed-jobs
// calls. Both tiers reach GitHub through the same handleEviction → rerunFailedJobs
// path, so one counter is the observable for "recovery fired" on either.
func rerunCounter(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			calls.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestAGC_Drain_ClassicWorkerEviction_DoesNotRerun measures the classic tier's
// no-terminal-phase deletion path. The provisioner is wired with the production
// InformerPodWaiter — not the poll fallback — because the two are only interchangeable
// for a pod that reaches a terminal phase, and this pod never does.
//
// The measured outcome: the eviction removes the pod, the waiter reports the vanished
// pod as PodSucceeded with no deletion mark, provision() takes no recovery branch, and
// no rerun is requested. This is the side of the Q502 boundary that stays unrecovered:
// a worker that vanished without a terminal phase never ran its job to a reportable
// end, so there is no failed job for rerun-failed-jobs to act on. The recovered side —
// the terminal phase publishing while the deletion is in flight — is
// TestAGC_Drain_ClassicWorkerTerminalWithMark_Reruns below.
func TestAGC_Drain_ClassicWorkerEviction_DoesNotRerun(t *testing.T) {
	const nsName = "agc-drain-classic"
	const rgName = "drain-rg"
	createNSForAGC(t, nsName)

	fakeGitHub, rerunCalls := rerunCounter(t)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL: fakeGitHub.URL,
		// A budget of 2 means an absent rerun is the drain path's own doing, not a
		// budget the test starved. The eviction test next door fires on the same
		// wiring with maxEvictionRetries=1.
		maxEvictionRetries: 2,
		informerWaiter:     true,
	})

	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner(rgName)) >= 1
	}, 15*time.Second, time.Millisecond, "%s session should register", rgName)

	// A complete run identity, so a missing rerun cannot be explained away as
	// handleEviction's runID == "" early return.
	brokerStub.SetAcquireJobResponse(map[string]interface{}{
		"plan":        map[string]string{"planId": "drain-plan-1"},
		"contextData": runIdentityContext("owner/repo", "4210"),
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	sid := enqueueJobOnOwnerSession(15*time.Second, rgName, map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found %s session to enqueue on", rgName)

	pod := waitForWorkerPod(t, nsName, rgName)
	require.Empty(t, pod.Status.Reason,
		"the worker pod must start out with no Status.Reason, or the assertion below proves nothing")

	// Drain the node the pod is on, as far as this pod is concerned.
	evictPod(t, &pod)
	requirePodGone(t, nsName, pod.Name)

	// The job Secret is deleted on provision()'s way out, so its absence is the signal
	// that the whole handler ran to completion — including step 7, the eviction branch.
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": rgName},
		); err != nil {
			return false
		}
		for _, s := range secrets.Items {
			if strings.HasPrefix(s.Name, "job-") {
				return false
			}
		}
		return true
	}, 20*time.Second, 50*time.Millisecond,
		"the job Secret must be reclaimed, which is how we know provision() reached its eviction branch")

	// The measurement. handleEviction waits out evictionRetryDelay before calling
	// GitHub, so give a fired rerun room to appear before concluding none did.
	assert.Never(t, func() bool { return rerunCalls.Load() > 0 }, 3*time.Second, 100*time.Millisecond,
		"a classic worker deleted without ever running its container must not reach "+
			"disruption recovery: Q502's deletion arm requires a recorded container exit "+
			"that the mark predates, and this pod has neither a terminal phase nor an exit")
}

// holdWithFinalizer pins the pod in the API through its deletion, so the object
// sequence a real kubelet produces on a drained running worker — deletionTimestamp
// first, terminal phase second — can be reproduced on an apiserver that has no kubelet
// to hold the object open. The cleanup removes the finalizer so the pod (and its
// namespace) can be reclaimed.
func holdWithFinalizer(t *testing.T, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod))
	pod.Finalizers = append(pod.Finalizers, "test.actions-gateway.com/hold")
	require.NoError(t, k8sClient.Update(ctx, &pod))
	t.Cleanup(func() {
		bg := context.Background()
		var fresh corev1.Pod
		if err := k8sClient.Get(bg, types.NamespacedName{Namespace: namespace, Name: name}, &fresh); err != nil {
			return
		}
		fresh.Finalizers = nil
		_ = k8sClient.Update(bg, &fresh)
	})
}

// publishTerminalFailure writes the terminal status a real kubelet publishes as a
// drained worker's container exits: PodFailed, an empty reason, and a container
// termination record. Note the venue's timestamp shape: envtest pods are unscheduled,
// so their eviction collapses grace to zero and deletionTimestamp equals the request
// time. A real kubelet's mark sits a whole grace period later than the exit a
// SIGTERM-honouring runner records — the offset the detection subtracts back out
// (deletionRequestedAt) and E2E_AGC_ScaleSetRecovery pins on a real cluster (Q519).
func publishTerminalFailure(t *testing.T, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = ""
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   1,
			FinishedAt: metav1.Now(),
		}},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, &pod),
		"the drained worker's terminal phase must be writable, or the test cannot pose its question")
}

// TestAGC_Drain_ClassicWorkerTerminalWithMark_Reruns is the classic half of Q502: the
// recovered side of the drain boundary. The worker's terminal phase publishes while its
// deletion is in flight — the shape Q459 measured a real drain of a running worker
// producing — and the InformerPodWaiter carries the mark out of the wait, provision()
// takes the deletion arm, and the interrupted run is re-run.
func TestAGC_Drain_ClassicWorkerTerminalWithMark_Reruns(t *testing.T) {
	const nsName = "agc-drain-classic-mark"
	const rgName = "drain-mark-rg"
	createNSForAGC(t, nsName)

	fakeGitHub, rerunCalls := rerunCounter(t)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{
		githubAPIURL:       fakeGitHub.URL,
		maxEvictionRetries: 2,
		informerWaiter:     true,
	})

	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner(rgName)) >= 1
	}, 15*time.Second, time.Millisecond, "%s session should register", rgName)

	brokerStub.SetAcquireJobResponse(map[string]interface{}{
		"plan":        map[string]string{"planId": "drain-mark-plan-1"},
		"contextData": runIdentityContext("owner/repo", "5020"),
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	sid := enqueueJobOnOwnerSession(15*time.Second, rgName, map[string]bool{}, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, sid, "should have found %s session to enqueue on", rgName)

	pod := waitForWorkerPod(t, nsName, rgName)

	// The kubelet's sequence, reproduced: the drain's eviction sets the deletion mark
	// (the finalizer stands in for the kubelet keeping the object alive), then the
	// container exits and the terminal phase publishes.
	holdWithFinalizer(t, nsName, pod.Name)
	evictPod(t, &pod)
	publishTerminalFailure(t, nsName, pod.Name)

	// The measurement: the run is re-run — the whole point of Q502 — and exactly once,
	// one drain spending one slot of the run's retry budget.
	require.Eventually(t, func() bool { return rerunCalls.Load() >= 1 }, 20*time.Second, 50*time.Millisecond,
		"a drained classic worker whose terminal phase published with the deletion mark "+
			"must have its run re-run automatically")
	assert.Never(t, func() bool { return rerunCalls.Load() > 1 }, 3*time.Second, 100*time.Millisecond,
		"one drain must spend exactly one slot of the run's retry budget")
}

// TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover is the no-terminal-phase
// measurement on the tier Q417 built. It is deliberately the twin of
// TestV2_RunnerSet_ScaleSet_EvictedWorkerTriggersRerun: same harness, same real
// provisioning path, same worker pod — and the single difference is that the pod
// vanishes via the eviction subresource without ever publishing a terminal phase. That
// is what turns a rerun into no rerun: with no terminal phase there is no reportable
// failure, so Q502's deletion arm (which requires PodFailed with the mark) never fires.
func TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover(t *testing.T) {
	const ns = "v2-rs-ss-drain"
	const label = "linux-drain"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	fakeGitHub, rerunCalls := rerunCounter(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-drain", ns, "gw", label, 3)
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	// A long TTL keeps the reaper from deleting the pod out from under the recovery
	// pass, so an absent rerun is the drain path's doing rather than lost evidence.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-drain", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-drain"}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	// The recoverable identity is present — this pod is exactly the one the eviction
	// twin re-runs. Only the exit path differs.
	require.NotEmpty(t, pod.Annotations[provisioner.AnnotationRunID],
		"the assignment's workflowRunId must have reached the pod, or no rerun could fire either way")

	evictPod(t, &pod)
	requirePodGone(t, ns, pod.Name)

	// The measurement. RecoverEvictedScaleSetWorkers runs every reconcile and lists from
	// the informer cache; an evicted-by-API pod that published no terminal phase is not
	// in that list at all, and would not pass disruptionAwaitingRecovery if it were —
	// the deletion arm requires PodFailed.
	//
	// This is also a boundary Q497 had to not cross. The eviction API stamps the SAME
	// DisruptionTarget condition a preemption does, with its own reason — so a detector
	// that keyed on the condition type alone would have recovered this shape by
	// accident. The drain path is instead gated on the deletion mark at terminal
	// publish (Q502), which this pod never reaches.
	assert.Never(t, func() bool { return rerunCalls.Load() > 0 }, 3*time.Second, 100*time.Millisecond,
		"a scale-set worker deleted without ever publishing a terminal phase must not reach "+
			"disruption recovery: there is no reportable failure to re-run")
}

// TestAGC_Drain_ScaleSetWorkerTerminalWithMark_Recovers is the scale-set half of Q502:
// the drained worker's terminal phase publishes while its deletion is in flight, the
// reconciler's worker-pod watch admits the phase change, and the recovery scan finds
// the Failed-with-mark pod inside its teardown window, claims it, and re-runs the
// interrupted run off the identity annotations.
func TestAGC_Drain_ScaleSetWorkerTerminalWithMark_Recovers(t *testing.T) {
	const ns = "v2-rs-ss-drain-mark"
	const label = "linux-drain-mark"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	fakeGitHub, rerunCalls := rerunCounter(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-drain-mark", ns, "gw", label, 3)
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-drain-mark", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-drain-mark"}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	require.NotEmpty(t, pod.Annotations[provisioner.AnnotationRunID],
		"the assignment's workflowRunId must have reached the pod, or no rerun could fire either way")

	// The kubelet's sequence, reproduced: mark first, terminal phase second (with the
	// container exit the scan orders the mark against).
	holdWithFinalizer(t, ns, pod.Name)
	evictPod(t, &pod)
	publishTerminalFailure(t, ns, pod.Name)

	// The measurement.
	require.Eventually(t, func() bool { return rerunCalls.Load() >= 1 }, 30*time.Second, 100*time.Millisecond,
		"a drained scale-set worker whose terminal phase published with the deletion mark "+
			"must have its run re-run automatically")

	// At-most-once across the reconciles the teardown window produces.
	assert.Never(t, func() bool { return rerunCalls.Load() > 1 }, 5*time.Second, 100*time.Millisecond,
		"one drain must spend exactly one slot of the run's retry budget, however many "+
			"reconciles observe the terminating pod")

	// The claim annotation is what enforces that, including across a restart or a
	// second replica.
	var claimed corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, &claimed))
	assert.Contains(t, claimed.Annotations, provisioner.AnnotationEvictionHandledAt,
		"the drained pod must carry the handled stamp, or a later reconcile would re-run its job again")
}

// TestAGC_Preemption_ScaleSetWorker_IsRecovered is the Q497 twin of the drain test
// above, and the two together are the whole scope statement: same harness, same real
// provisioning path, same worker pod, same graceful removal — and the ONE difference is
// the reason on the DisruptionTarget condition. That single substitution is what turns
// no rerun into a rerun, which is exactly the claim (`PreemptionByScheduler` has one
// writer, so it is safe to recover on where a bare deletionTimestamp is not).
//
// What this venue can and cannot settle. envtest runs an apiserver but no scheduler, so
// nothing here can preempt anything; the condition is stamped by hand, with the shape
// E2E_AGC_WorkerPreemption measured a real kube-scheduler writing. What it does settle
// is everything downstream of the marker on a REAL apiserver: the worker-pod watch
// waking the reconciler on a non-phase-changing update, the recovery scan finding a
// terminating pod, the optimistic-lock claim landing on it, and the identity coming off
// its annotations.
func TestAGC_Preemption_ScaleSetWorker_IsRecovered(t *testing.T) {
	const ns = "v2-rs-ss-preempt"
	const label = "linux-preempt"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	fakeGitHub, rerunCalls := rerunCounter(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-preempt", ns, "gw", label, 3)
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-preempt", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-preempt"}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	require.NotEmpty(t, pod.Annotations[provisioner.AnnotationRunID],
		"the assignment's workflowRunId must have reached the pod, or no rerun could fire either way")

	// The substitution. The phase is left exactly as it is — Pending, with no container
	// ever started, which is the shape the real preemption e2e reproduces and the shape
	// no phase-based detection could ever act on.
	markPreempted(t, &pod)

	// The measurement.
	require.Eventually(t, func() bool { return rerunCalls.Load() >= 1 }, 30*time.Second, 100*time.Millisecond,
		"a preempted scale-set worker's run must be re-run automatically; without it, oversubscription "+
			"costs a manual re-run for every displaced job")

	// At-most-once across the many reconciles the pod's remaining lifetime produces.
	assert.Never(t, func() bool { return rerunCalls.Load() > 1 }, 5*time.Second, 100*time.Millisecond,
		"one preemption must spend exactly one slot of the run's retry budget, however many "+
			"reconciles observe the victim")

	// The claim annotation is what enforces that, including across a restart or a second
	// replica, so it must actually be on the pod rather than implied by the count.
	var claimed corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, &claimed))
	assert.Contains(t, claimed.Annotations, provisioner.AnnotationEvictionHandledAt,
		"the preempted pod must carry the handled stamp, or a later reconcile would re-run its job again")
}

// markPreempted stamps the DisruptionTarget condition kube-scheduler writes on a
// preemption victim. envtest has no scheduler, so this is the hand-written stand-in for
// the one thing this venue cannot produce; E2E_AGC_WorkerPreemption pins that a real
// scheduler writes exactly this.
//
// The pod is deliberately NOT deleted afterwards. A real victim stays readable for its
// whole termination grace period, and that window is precisely what the recovery scan
// runs in — deleting here would test a race the scheduler does not actually impose.
func markPreempted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	var fresh corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &fresh))
	fresh.Status.Conditions = append(fresh.Status.Conditions, corev1.PodCondition{
		Type:               corev1.DisruptionTarget,
		Status:             corev1.ConditionTrue,
		Reason:             corev1.PodReasonPreemptionByScheduler,
		Message:            "Pod was preempted by a higher-priority pod",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, k8sClient.Status().Update(ctx, &fresh),
		"the preemption marker must be writable, or the test cannot pose its question")
}
