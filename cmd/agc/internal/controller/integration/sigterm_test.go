//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestAGC_SIGTERM_DeletesAllSessions verifies that cancelling the reconciler context
// (simulating SIGTERM) causes all registered sessions to be deleted via DELETE /session.
func TestAGC_SIGTERM_DeletesAllSessions(t *testing.T) {
	// Detect goroutine leaks after this test.
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("sigs.k8s.io/controller-runtime/pkg/internal/testing/process.(*Process).Start.func1"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).ListAndWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).watchHandler"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).processLoop"),
	)

	const nsName = "agc-sigterm-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "sigterm-rg", 3)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	cancelMgr := startAGCReconciler(t)

	// Wait for the initial session (permanent baseline listener).
	require.Eventually(t, func() bool {
		return len(brokerStub.RegisteredSessions()) >= 1
	}, 15*time.Second, 200*time.Millisecond, "initial session should register")

	// Burst to 3 sessions by sequentially enqueueing 2 jobs.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		id := enqueueJobWhenSessionAvailable(15*time.Second, seen, broker.RunnerJobRequestBody{})
		require.NotEmpty(t, id, "new session must appear to enqueue job %d", i+1)
		seen[id] = true
		// Wait for the next spawned session before enqueueing the next job.
		require.Eventually(t, func() bool {
			return len(brokerStub.RegisteredSessions()) >= i+2
		}, 10*time.Second, 100*time.Millisecond)
	}

	// Capture the session IDs before cancellation.
	sessionIDs := brokerStub.RegisteredSessions()
	require.GreaterOrEqual(t, len(sessionIDs), 2,
		"at least 2 sessions must be active before SIGTERM")

	// Simulate SIGTERM: cancel the manager context.
	cancelMgr()

	// Assert all registered sessions are deleted via DELETE /session.
	for _, sid := range sessionIDs {
		deleted := brokerStub.WaitForSessionDelete(sid, 10*time.Second)
		assert.Truef(t, deleted, "session %q should be deleted on SIGTERM", sid)
	}
}

// Ensure broker import is used.
var _ = broker.RunnerJobRequestBody{}
