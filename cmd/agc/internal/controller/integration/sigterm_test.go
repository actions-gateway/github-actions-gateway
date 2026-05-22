//go:build integration

package integration_test

import (
	"net/http"
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
	// Note: IgnoreAnyFunction/IgnoreTopFunction use exact function-name matching.
	defer goleak.VerifyNone(t,
		// envtest process-watcher goroutines (kube-apiserver + etcd; live for the whole suite).
		goleak.IgnoreAnyFunction("sigs.k8s.io/controller-runtime/pkg/internal/testing/process.(*State).Start.func1"),
		// client-go informer goroutines managed by the controller-runtime manager.
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).ListAndWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).watchHandler"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).processLoop"),
		// Broker stub server: accept loop + per-connection serve goroutines (global throughout suite).
		goleak.IgnoreAnyFunction("net/http/httptest.(*Server).goServe.func1"),
		goleak.IgnoreAnyFunction("net/http.(*conn).serve"),
		// k8s client HTTP/2 connection to the kube-apiserver — suite-level, lives on the
		// k8s client's own transport which we have no handle on from the test.
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
	)

	const nsName = "agc-sigterm-test"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "sigterm-rg", 3)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	cancelMgr, mgrDone := startAGCReconciler(t)

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

	// Simulate SIGTERM: cancel the manager context and wait for full shutdown
	// before goleak's defer runs. Then drain idle keep-alive connections so
	// persistConn goroutines exit before goleak checks.
	cancelMgr()
	<-mgrDone
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()

	// Assert all registered sessions are deleted via DELETE /session.
	for _, sid := range sessionIDs {
		deleted := brokerStub.WaitForSessionDelete(sid, 10*time.Second)
		assert.Truef(t, deleted, "session %q should be deleted on SIGTERM", sid)
	}
}

// Ensure broker import is used.
var _ = broker.RunnerJobRequestBody{}
