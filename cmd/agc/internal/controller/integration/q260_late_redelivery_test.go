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

// TestAGC_Q260_LateRedeliveryAfterCompletionDedups is the envtest regression for
// the Q260 redelivery residual. The planID-keyed dedup (fixed and live-validated
// in #508/#511) collapses the burst fan-out at job start, but it released the
// planID claim the instant the winner COMPLETED — while the winner's terminal
// worker pod lingers for completedPodTTL before the reaper GCs it. A LATE GitHub
// redelivery of that already-completed planID then passed the freshly-released
// claim gate, re-provisioned, and collided on `create Pod runner-…-<planID>` with
// the lingering Completed pod ("already exists") — the residual observed in
// dogfood re-route #3.
//
// The fix retains the planID claim for ClaimLinger (the owner's completedPodTTL)
// past completion, so a redelivery arriving while the winner's pod still lingers
// is deduped (counted on the duplicate-delivery metric) and never re-provisions.
//
// The test drives one session to win, provision, and COMPLETE (its pod set to
// Succeeded so provision returns and the pod lingers), then delivers the same
// planID again and asserts it is deduped rather than colliding on a second Pod.
//
// This fails against the pre-fix behavior (delete-on-completion, commit 1f4111b):
// the late redelivery would reclaim the freed planID, re-enter the provisioner,
// and hit the real Pod AlreadyExists — the duplicate-delivery counter would stay
// flat, so the delta asserted below never arrives and the test times out.
func TestAGC_Q260_LateRedeliveryAfterCompletionDedups(t *testing.T) {
	const (
		nsName = "agc-q260-late"
		rgName = "late-rg"
		planID = "shared-late-plan"
	)
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// Force every AcquireJob to return the SAME planID, so the winner and the late
	// redelivery resolve to one job identity (the shape the claim keys on). The
	// group omits completedPodTTL, so it defaults to 5m — the pod lingers after
	// completion and the claim's linger covers that whole window.
	brokerStub.SetAcquireJobResponse(map[string]any{
		"plan": map[string]string{"planId": planID},
	})
	t.Cleanup(func() { brokerStub.SetAcquireJobResponse(nil) })

	m := sharedListenerMetrics()
	startAGCReconcilerWithProvisioner(t, provisionerOptions{metrics: m})

	// The winner: deliver a job to the baseline session. It acquires (planID
	// shared-late-plan), claims it, and provisions the worker Secret + pod, then
	// blocks in waitForCompletion until we advance the pod below.
	winnerID := enqueueJobOnOwnerSession(15*time.Second, rgName, nil, broker.RunnerJobRequestBody{RunnerRequestID: "winner"})
	require.NotEmpty(t, winnerID, "expected a baseline session to deliver the winning job to")

	// Wait for the winner's worker pod (proof it holds the planID claim and has
	// provisioned).
	var podName string
	require.Eventually(t, func() bool {
		podName = firstWorkerPod(t, nsName, rgName)
		return podName != ""
	}, 15*time.Second, 25*time.Millisecond, "winner should provision its worker pod")
	require.Equal(t, 1, countJobSecrets(t, nsName, rgName), "winner stages exactly one job Secret")

	// Complete the winner: advance its pod to Succeeded (envtest has no kubelet).
	// waitForCompletion returns, provision deletes the Secret, and the pod lingers
	// (completedPodTTL default 5m) — after which handleJob releases the claim into
	// its linger window.
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: podName}, &pod))
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	// The winner's Secret is deleted once provision returns — the completion
	// signal. Its terminal pod, however, still lingers (not reaped).
	require.Eventually(t, func() bool {
		return countJobSecrets(t, nsName, rgName) == 0
	}, 15*time.Second, 25*time.Millisecond, "winner's job Secret is deleted on completion")
	require.Equal(t, 1, countWorkerPods(t, nsName, rgName),
		"the winner's Completed pod must still linger (not yet reaped) for the redelivery to race")

	dupBefore := testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName))

	// The late redelivery: GitHub redelivers the SAME planID (distinct
	// RunnerRequestID) to a live session of this group AFTER the winner completed
	// but while its pod still lingers. It acquires (same planID), finds the claim
	// still held (lingering), and is deduped — it must NOT re-provision or collide
	// on the lingering pod.
	//
	// Never onto the winner's own session. Its AcquireJob spent that single-use JIT
	// agent, so the goroutine DELETEs it and opens a fresh one (Q114) as soon as the
	// job concludes, which the Secret deletion above says has happened. The stub
	// queues jobs per session ID and re-offers nothing, so a redelivery landing in
	// that queue waits for a poll that never comes (Q1008).
	lateID := enqueueJobOnOwnerSession(15*time.Second, rgName,
		map[string]bool{winnerID: true}, broker.RunnerJobRequestBody{RunnerRequestID: "loser-late"})
	require.NotEmpty(t, lateID, "expected a live session to deliver the late redelivery to")

	// The duplicate-delivery counter rises: the late redelivery was deduped on the
	// lingering planID claim (fails against 1f4111b, which freed the claim on
	// completion and so re-provisioned into the Pod AlreadyExists collision).
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues(nsName, rgName)) >= dupBefore+1
	}, 15*time.Second, 25*time.Millisecond,
		"a late redelivery during the pod-linger window must be deduped (fails against 1f4111b)")

	// Exactly one worker pod for this planID ever exists — the redelivery never
	// attempted a second `create Pod`.
	assert.Equal(t, 1, countWorkerPods(t, nsName, rgName),
		"exactly one worker pod must exist; the late redelivery must not create a second")
}

// firstWorkerPod returns the name of the first worker pod (runner-* prefix) owned
// by the RunnerGroup, or "" if none exists yet.
func firstWorkerPod(t *testing.T, namespace, rgName string) string {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"actions-gateway/runner-group": rgName}))
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "runner-") {
			return p.Name
		}
	}
	return ""
}

// countWorkerPods returns the number of worker pods (runner-* prefix) owned by the
// RunnerGroup, excluding any already being deleted.
func countWorkerPods(t *testing.T, namespace, rgName string) int {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"actions-gateway/runner-group": rgName}))
	n := 0
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "runner-") && p.DeletionTimestamp.IsZero() {
			n++
		}
	}
	return n
}
