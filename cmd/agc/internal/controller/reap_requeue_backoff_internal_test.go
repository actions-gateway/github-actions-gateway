package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func backoffTestRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "group"}}
}

// TestReconcileRateLimiter_RetryDelayStaysBounded is the reaper's liveness
// guarantee stated at the work queue: however long a run of reconcile errors
// lasts, the next attempt is never more than retryBackoffCap away.
//
// It matters because a reap deadline has no second carrier. The reconcile that
// sees a Pending worker pod returns the time until it is due as RequeueAfter,
// controller-runtime drops RequeueAfter on any reconcile that also returns an
// error, and a pod sitting Pending raises no further watch event — so a run of
// errors leaves the rate-limited retry as the only thing that can still deliver
// the reap. Under client-go's default cap that retry reaches 1000s.
func TestReconcileRateLimiter_RetryDelayStaysBounded(t *testing.T) {
	rl := reconcileRateLimiter()
	req := backoffTestRequest()

	var worst time.Duration
	for i := 0; i < 64; i++ {
		if d := rl.When(req); d > worst {
			worst = d
		}
	}
	require.Equal(t, retryBackoffCap, worst,
		"64 consecutive reconcile errors must not push the retry past retryBackoffCap")

	// What the cap replaced, so the size of the gap is on the record: the same
	// run of failures against client-go's default escalates two orders of
	// magnitude further, which is how long a stuck-Pending worker could hold its
	// concurrency slot past pendingPodDeadline.
	def := workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	var defWorst time.Duration
	for i := 0; i < 64; i++ {
		if d := def.When(req); d > defWorst {
			defWorst = d
		}
	}
	require.Greater(t, defWorst, retryBackoffCap,
		"the default limiter is what this cap exists to replace")
}

// TestReconcileRateLimiter_RampAndForgetUnchanged pins the two properties the
// cap must not disturb: retries still start fast and grow, and a successful
// reconcile still clears the item's failure count.
func TestReconcileRateLimiter_RampAndForgetUnchanged(t *testing.T) {
	rl := reconcileRateLimiter()
	req := backoffTestRequest()

	first := rl.When(req)
	second := rl.When(req)
	require.Less(t, first, time.Second, "the first retry after an error is immediate-ish")
	require.Greater(t, second, first, "consecutive failures back off exponentially")

	for i := 0; i < 64; i++ {
		rl.When(req)
	}
	rl.Forget(req)
	require.Equal(t, first, rl.When(req),
		"a successful reconcile resets the item's failure count")
}
