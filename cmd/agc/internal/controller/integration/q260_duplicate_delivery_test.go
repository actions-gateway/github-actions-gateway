//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAGC_Q260_DuplicateDeliveryDedupsOnPlanID is the envtest regression for the
// Q260 re-fix. Under GitHub's broker fan-out, sibling sessions of one job receive
// DISTINCT RunnerRequestIDs, but the AcquireJob response carries one SHARED planID,
// and the per-job worker Secret is named "job-<planID>". Against the real API
// server, two siblings provisioning the same planID collide on Secret creation
// ("already exists"). The dedup must therefore key on planID (only known
// post-acquire), so exactly one sibling provisions and the rest are deduped.
//
// The test drives one session (RunnerRequestID "winner") to acquire, claim the
// planID, and provision the worker Secret+pod — then block in waitForCompletion
// (no kubelet terminates the Pending pod), holding the claim for the rest of the
// test. It then delivers the SAME job with a DISTINCT RunnerRequestID ("loser") to
// the replacement sibling session, and asserts the sibling is deduped on planID
// (the duplicate-delivery counter rises) rather than colliding on the Secret.
//
// This fails against the first fix (c850764), which keyed the claim on
// RunnerRequestID: the distinct-id loser would pass that gate, hit the real
// AlreadyExists on job-<planID>, and never increment the counter — so the delta
// asserted below would stay 0.
func TestAGC_Q260_DuplicateDeliveryDedupsOnPlanID(t *testing.T) {
	const (
		nsName = "agc-q260-dup"
		rgName = "dup-rg"
		planID = "shared-plan"
	)
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// Force every AcquireJob to return the SAME planID, reproducing the fan-out:
	// distinct RunnerRequestIDs (set per delivery below) resolving to one planID.
	brokerStub.SetAcquireJobResponse(map[string]any{
		"plan": map[string]string{"planId": planID},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	m := sharedListenerMetrics()
	startAGCReconcilerWithProvisioner(t, provisionerOptions{metrics: m})

	// The winner: deliver a job to the baseline session. It acquires (planID
	// shared-plan), claims it, provisions the worker Secret+pod, then blocks in
	// waitForCompletion — holding the planID claim for the rest of the test.
	winnerID := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{RunnerRequestID: "winner"})
	require.NotEmpty(t, winnerID, "expected a baseline session to deliver the winning job to")

	// The winner has provisioned once it created its worker Secret (named
	// "job-<safeName(planID)>", a deterministic hash of the planID) — proof it now
	// holds the planID claim.
	require.Eventually(t, func() bool {
		return countJobSecrets(t, nsName, rgName) >= 1
	}, 15*time.Second, 25*time.Millisecond, "winner should provision its job-<planID> Secret")

	// The winner's post-acquire SpawnReplacement brings a sibling session online to
	// receive the duplicate delivery.
	var siblingID string
	require.Eventually(t, func() bool {
		for _, id := range brokerStub.ActiveSessionsForOwner(rgName) {
			if id != winnerID {
				siblingID = id
				return true
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond, "winner's SpawnReplacement should bring a sibling session online")

	dupBefore := testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName))
	acquiresBefore := brokerStub.AcquireJobCalls()
	completeBefore := brokerStub.CompleteJobCalls()

	// The loser: deliver the SAME job with a DISTINCT RunnerRequestID to the sibling.
	// It acquires (same planID), finds the planID already claimed by the winner, and
	// is deduped — it must NOT collide on the Secret.
	brokerStub.EnqueueJob(siblingID, broker.RunnerJobRequestBody{RunnerRequestID: "loser"})

	// The duplicate-delivery counter rises: the sibling was deduped on planID.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName)) >= dupBefore+1
	}, 15*time.Second, 25*time.Millisecond, "the distinct-RunnerRequestID sibling must be deduped on planID (fails against c850764)")

	// The dedup is POST-acquire: the loser did run AcquireJob (planID is only known
	// then), distinguishing this from the pre-acquire RunnerRequestID gate.
	assert.GreaterOrEqual(t, brokerStub.AcquireJobCalls(), acquiresBefore+1,
		"the loser acquires before the planID dedup gate (post-acquire dedup)")

	// Exactly one worker Secret for this planID ever exists — the real API server
	// never saw a second create for the shared job-<planID> Secret.
	assert.Equal(t, 1, countJobSecrets(t, nsName, rgName),
		"exactly one job-<planID> worker Secret must exist (no duplicate provision)")

	// Off by default: with FanoutCompletion unset (the production default), no
	// completejob is issued for the deduped sibling — the winner blocks (its pod stays
	// Pending), so it never reaches its fan-out, and the loser never completes itself.
	assert.Equal(t, completeBefore, brokerStub.CompleteJobCalls(),
		"no sibling completejob must be issued when the guard is off (default)")
}

// TestAGC_Q260_WinnerCompletesDedupedSiblingDelivery is the envtest regression for
// the Q260 Option A fix: when FanoutCompletion is enabled, a deduped sibling
// delivery is not skipped-and-forgotten — the WINNER, when its job finishes, fans a
// completejob out to the sibling's acquired-but-unrun assignment so GitHub does not
// cancel the whole job at its ~15-minute unstarted-job timeout even after the winner
// completed it. The result reported is the winner's pod-phase proxy (its pod
// Succeeded here → "succeeded"), NOT the rejected #513 immediate-"skipped" path.
//
// It mirrors TestAGC_Q260_DuplicateDeliveryDedupsOnPlanID up to the dedup (winner
// claims the shared planID and provisions; a DISTINCT-RunnerRequestID sibling
// "loser" is deduped and registered), then drives the winner's pod to Succeeded so
// the winner concludes and fans completion out. It asserts exactly one completejob
// keyed on the SIBLING's own jobID ("loser") with result "succeeded".
func TestAGC_Q260_WinnerCompletesDedupedSiblingDelivery(t *testing.T) {
	const (
		nsName = "agc-q260-fanout"
		rgName = "fanout-rg"
		planID = "shared-plan-fanout"
	)
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	brokerStub.SetAcquireJobResponse(map[string]any{
		"plan": map[string]string{"planId": planID},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	m := sharedListenerMetrics()
	// Opt into the guarded behavior for this test only.
	startAGCReconcilerWithProvisioner(t, provisionerOptions{metrics: m, fanoutCompletion: true})

	// The winner acquires the shared planID, claims it, and provisions its worker
	// Secret + pod, then blocks in waitForCompletion until we advance the pod below.
	winnerID := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{RunnerRequestID: "winner"})
	require.NotEmpty(t, winnerID, "expected a baseline session to deliver the winning job to")

	var podName string
	require.Eventually(t, func() bool {
		podName = firstWorkerPod(t, nsName, rgName)
		return podName != ""
	}, 15*time.Second, 25*time.Millisecond, "winner should provision its worker pod")

	// The winner's SpawnReplacement brings a sibling online for the duplicate.
	var siblingID string
	require.Eventually(t, func() bool {
		for _, id := range brokerStub.ActiveSessionsForOwner(rgName) {
			if id != winnerID {
				siblingID = id
				return true
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond, "winner's SpawnReplacement should bring a sibling session online")

	dupBefore := testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName))
	completeBefore := brokerStub.CompleteJobCalls()

	// The loser: SAME planID, DISTINCT RunnerRequestID, delivered to the sibling. It
	// is deduped and registered on the winner's claim — but NOT completed yet (the
	// winner is still running).
	brokerStub.EnqueueJob(siblingID, broker.RunnerJobRequestBody{RunnerRequestID: "loser"})
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName)) >= dupBefore+1
	}, 15*time.Second, 25*time.Millisecond, "the distinct-RunnerRequestID sibling must be deduped on planID")
	assert.Equal(t, completeBefore, brokerStub.CompleteJobCalls(),
		"the sibling must NOT be completed while the winner is still running (not the rejected #513 immediate path)")

	// Complete the winner: advance its pod to Succeeded (envtest has no kubelet).
	// waitForCompletion returns, the winner concludes, and fans completion out to the
	// registered sibling delivery.
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: podName}, &pod))
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// The winner fans a completejob out to the sibling's assignment on completion.
	require.Eventually(t, func() bool {
		return brokerStub.CompleteJobCalls() >= completeBefore+1
	}, 15*time.Second, 25*time.Millisecond, "the winner must complete the deduped sibling delivery on finish")

	// The completejob is keyed on the SIBLING's own jobID (not the winner's), with the
	// winner's pod-phase proxy result "succeeded" — resolving only that assignment.
	last, ok := brokerStub.LastCompleteJob()
	require.True(t, ok, "a completejob request should have been recorded")
	assert.Equal(t, "loser", last.JobID, "completejob must key on the sibling's own delivery jobID")
	assert.Equal(t, planID, last.PlanID, "completejob must carry the shared planID")
	assert.Equal(t, broker.TaskResultSucceeded, last.Result, "the sibling is completed with the winner's pod-phase proxy (succeeded)")

	// The completion counter records the outcome as completed.
	assert.GreaterOrEqual(t, testutil.ToFloat64(m.AbandonedDeliveryCompletionsTotal.WithLabelValues(nsName, rgName, "completed")), float64(1),
		"the fan-out completion counter should record a completed outcome")

	// Still no duplicate provision: the winner's Secret is deleted on completion, and
	// the sibling never provisioned a second pod — exactly the winner's Succeeded pod
	// lingers (completedPodTTL default 5m, not yet reaped).
	assert.Equal(t, 1, countWorkerPods(t, nsName, rgName),
		"exactly one worker pod must exist (the winner's; the deduped sibling never provisioned)")
}

// countJobSecrets returns the number of worker (job-payload) Secrets in the
// namespace owned by the RunnerGroup. Worker Secrets are named "job-<planID hash>"
// and carry the runner-group label; agent Secrets do not use the "job-" prefix.
func countJobSecrets(t *testing.T, namespace, rgName string) int {
	t.Helper()
	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &secrets,
		client.InNamespace(namespace),
		client.MatchingLabels{"actions-gateway/runner-group": rgName}))
	n := 0
	for _, s := range secrets.Items {
		if strings.HasPrefix(s.Name, "job-") {
			n++
		}
	}
	return n
}
