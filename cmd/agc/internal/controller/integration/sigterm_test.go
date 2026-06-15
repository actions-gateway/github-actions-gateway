//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
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
		// mgr.Start() returns before these fully exit — they shut down asynchronously.
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).ListAndWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).watchHandler"),
		// handleAnyWatch and startResync were added in newer client-go versions;
		// same category as ListAndWatch/watchHandler above.
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.handleAnyWatch"),
		goleak.IgnoreTopFunction("k8s.io/client-go/tools/cache.(*Reflector).startResync"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).processLoop"),
		// controller-runtime priority queue (replaces client-go workqueue in ≥ v0.23).
		// Its background workers shut down asynchronously after mgr.Stop() returns.
		// handleReadyItems can be mid-btree-send; handleWaitingItems and handleAddBuffer
		// can be blocked acquiring the queue mutex (top frame = sync.Mutex.Lock). The
		// latter two's top frame varies, so match them by any frame rather than top frame.
		goleak.IgnoreTopFunction("sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue.(*priorityqueue[...]).handleReadyItems.func1.1"),
		goleak.IgnoreAnyFunction("sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue.(*priorityqueue[...]).handleWaitingItems"),
		goleak.IgnoreAnyFunction("sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue.(*priorityqueue[...]).handleAddBuffer"),
		// Broker stub server: accept loop + per-connection serve goroutines (global throughout suite).
		goleak.IgnoreAnyFunction("net/http/httptest.(*Server).goServe.func1"),
		goleak.IgnoreAnyFunction("net/http.(*conn).serve"),
		// k8s client HTTP/2 and HTTP/1.1 connection goroutines to the kube-apiserver —
		// suite-level, tied to the envtest REST client; we have no handle on them from the test.
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientStream).writeRequest"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
	)

	const nsName = "agc-sigterm-test"
	const rgName = "sigterm-rg"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 3)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rg) })

	cancelMgr, mgrDone := startAGCReconciler(t)

	// Scope every session operation to this RunnerGroup's owner ("sigterm-rg-").
	// Because the broker stub is shared across the whole package, the global
	// RegisteredSessions list can include sessions other tests left active; a
	// previous snapshot-and-diff approach was fragile (Q120). Owner-scoping is the
	// deterministic isolation: this RunnerGroup's name is unique to this test, so
	// ActiveSessionsForOwner returns exactly the sessions we created — never a
	// sibling's, and never one whose goroutine has already exited.
	require.Eventually(t, func() bool {
		return len(brokerStub.ActiveSessionsForOwner(rgName)) >= 1
	}, 15*time.Second, 1*time.Millisecond, "initial session should register")

	// Burst to 3 sessions by sequentially enqueueing 2 jobs, each onto a fresh
	// sigterm-rg session. seen tracks the sessions we have already enqueued on so
	// each job lands on a distinct session.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		id := enqueueJobOnOwnerSession(15*time.Second, rgName, seen, broker.RunnerJobRequestBody{})
		require.NotEmpty(t, id, "new session must appear to enqueue job %d", i+1)
		seen[id] = true
		// Wait for the next spawned session before enqueueing the next job.
		require.Eventually(t, func() bool {
			return len(brokerStub.ActiveSessionsForOwner(rgName)) >= i+2
		}, 10*time.Second, 1*time.Millisecond)
	}

	// Capture this RunnerGroup's active session IDs before cancellation.
	sessionIDs := brokerStub.ActiveSessionsForOwner(rgName)
	require.GreaterOrEqual(t, len(sessionIDs), 2,
		"at least 2 sessions must be active before SIGTERM")

	// Wait for every session to have sent its first GET /message before firing
	// SIGTERM. RegisteredSessions() returns as soon as POST /session is processed
	// server-side, but the goroutine may still be reading the HTTP response. If
	// cancelMgr fires in that window the goroutine dies without running its
	// cleanup defer, so DELETE /session is never sent. WaitForFirstPoll confirms
	// the goroutine has fully started (passed createSession, registered the defer,
	// and entered the poll loop) so SIGTERM is guaranteed to trigger cleanup.
	for _, sid := range sessionIDs {
		require.Truef(t, brokerStub.WaitForFirstPoll(sid, 15*time.Second),
			"session %q should reach the poll loop before SIGTERM", sid)
	}

	// Simulate SIGTERM: cancel the manager context and wait for full shutdown.
	cancelMgr()
	<-mgrDone

	// Assert all registered sessions are deleted via DELETE /session.
	// WaitForSessionDelete is a channel-based wait (it returns the instant the
	// broker processes the DELETE, not on a poll tick), so the timeout is only a
	// safety ceiling, not the expected latency. DELETE happens asynchronously after
	// mgr.Start returns — under a CPU-starved 2-vCPU CI runner the last goroutine's
	// DELETE round-trip can lag well past a few seconds, which flaked the old 10s
	// ceiling (Q120). 30s gives generous headroom while staying inside the package's
	// 5m test timeout; a session genuinely failing to delete still fails fast on
	// the assert once the ceiling elapses.
	for _, sid := range sessionIDs {
		deleted := brokerStub.WaitForSessionDelete(sid, 30*time.Second)
		assert.Truef(t, deleted, "session %q should be deleted on SIGTERM", sid)
	}

	// Close idle keep-alive connections so persistConn goroutines exit before
	// goleak's defer runs. Called after WaitForSessionDelete so all DELETE
	// responses have been processed and the connections are now idle.
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}
