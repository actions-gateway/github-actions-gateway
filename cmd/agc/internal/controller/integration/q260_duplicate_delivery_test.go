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
