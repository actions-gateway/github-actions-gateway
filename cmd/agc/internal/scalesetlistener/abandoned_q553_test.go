package scalesetlistener_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tests for the Q553 assignment check. The Q551 re-offer machinery holds an assignment
// until it provisions or the queue reports it complete, and the backend does not always
// report a completion for a job it has stopped honouring — so the Listener re-offered a
// job that no longer existed, forever. Every re-offer that got through provisioned a
// worker with nothing to run, which is what stops a drain converging: the drain waits on
// worker pods, and the listener kept making them.
//
// scalesettest's DropAssignedJob is the lever: it ends an assignment server-side WITHOUT
// appending a JobCompleted, so the statistics are the only thing that changes — the shape
// the v1.3.0-rc.3 dogfood gate recorded (fifteen held against zero assigned).

// fastAssignmentCheck shortens the assignment-check interval so a test drives the path in
// test time; production reads the statistics once a minute while it is holding anything.
func fastAssignmentCheck(c *scalesetlistener.Config) {
	c.AssignmentCheckInterval = 20 * time.Millisecond
}

// newQuickPollServer is a fake whose empty poll returns promptly. The check runs once per
// poll cycle, so its real cadence is the slower of the configured interval and the poll
// window — a default 1s window would pace these tests at one reading per second whatever
// fastAssignmentCheck asks for, and would leave a test that waits for something NOT to
// happen passing on a window too short to have observed it.
func newQuickPollServer(t *testing.T) *scalesettest.Server {
	t.Helper()
	srv := scalesettest.New()
	srv.SetPollTimeout(20 * time.Millisecond)
	t.Cleanup(srv.Close)
	return srv
}

// stallJob enqueues a job and makes every runner name it could register conflict, so it
// lands in the deferred set instead of provisioning. Returns its jobID.
func stallJob(t *testing.T, srv *scalesettest.Server, ssID int, m *countingMetrics, want int) string {
	t.Helper()
	_, jobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + jobID)
	require.Eventually(t, func() bool { return m.deferredCount() == want }, 5*time.Second, 10*time.Millisecond,
		"the job that cannot register a runner name must be held for a re-offer")
	return jobID
}

// TestListener_AbandonsAssignmentGitHubNoLongerHas is the core Q553 fix. Once the scale
// set reports no assigned jobs at all, the assignment the Listener is still holding
// cannot exist, so it stops re-offering it — and never provisions the worker that a
// successful re-offer would otherwise create for a job with nothing to run.
func TestListener_AbandonsAssignmentGitHubNoLongerHas(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m,
		fastDeferredRetry, fastAssignmentCheck)
	capVal.set(5)

	jobID := stallJob(t, srv, ssID, m, 1)

	// GitHub loses the assignment and says nothing: no JobCompleted, only a statistics
	// snapshot that no longer counts it.
	require.True(t, srv.DropAssignedJob(ssID, jobID),
		"the fake must be able to drop the assignment the listener is holding")
	require.Zero(t, srv.AssignedJobCount(ssID), "the scale set now holds no assigned job")

	require.Eventually(t, func() bool { return m.deferredCount() == 0 }, 5*time.Second, 10*time.Millisecond,
		"an assignment the scale set no longer counts must leave the deferred set")
	assert.Equal(t, 1, m.abandonedCount(), "the abandoned assignment is counted as a loss, not a completion")
	assert.Zero(t, m.completedCount(), "nothing reported the job complete — that is the whole point")

	// The stall is over and stays over: no further re-offer, and no worker for a job
	// that does not exist.
	errsBefore := m.provisionErrors()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, errsBefore, m.provisionErrors(), "an abandoned assignment must not be re-offered")
	assert.Empty(t, prov.jobIDs(), "no worker is provisioned for a job GitHub no longer has")
}

// TestListener_KeepsAssignmentWhileGitHubStillHoldsOne is the negative half, and the one
// that makes the check server-authoritative rather than a timeout: a deferred job whose
// scale set still counts an assignment is held, however long it has been stalled. Reading
// zero is the only unambiguous state — a positive count says how many are live, never
// which — so the check must act on nothing else.
func TestListener_KeepsAssignmentWhileGitHubStillHoldsOne(t *testing.T) {
	srv := newQuickPollServer(t)

	// A worker that never reports its job complete, so the job it holds stays assigned
	// at GitHub for the whole test.
	prov := &recordingProvisioner{srv: srv, completeErr: true}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m,
		fastDeferredRetry, fastAssignmentCheck)
	capVal.set(5)

	_, runningJobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the healthy job must provision and stay assigned")

	stalledJobID := stallJob(t, srv, ssID, m, 1)
	require.NotEqual(t, runningJobID, stalledJobID)

	// Well past several assignment-check intervals, with the stalled job still held.
	time.Sleep(time.Second)
	assert.Equal(t, 1, m.deferredCount(),
		"a stalled job must stay held while the scale set still counts an assignment")
	assert.Zero(t, m.abandonedCount(), "nothing may be abandoned on a positive assigned count")

	// And it still runs when its conflict finally clears — the hold was doing its job.
	srv.ClearJITConfigConflicts()
	require.Eventually(t, func() bool { return m.provisionedCount() == 2 }, 5*time.Second, 10*time.Millisecond,
		"the held job must still provision once its conflict clears")
	assert.Zero(t, m.abandonedCount(), "a job that ran was never abandoned")
}

// TestListener_AbandonNeedsTwoZeroReadings pins the confirmation rule. A statistics count
// is server state an assignment may briefly lead, so one zero reading is not enough to
// conclude a job is gone — a job is abandoned only once a zero reading brackets it on
// both sides. Without this, a job deferred in the instant before a reading could be
// dropped while GitHub was still waiting for it to run.
func TestListener_AbandonNeedsTwoZeroReadings(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m,
		fastDeferredRetry, fastAssignmentCheck)
	capVal.set(5)

	jobID := stallJob(t, srv, ssID, m, 1)
	refreshesBefore := srv.RefreshSessionCalls()
	require.True(t, srv.DropAssignedJob(ssID, jobID))

	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the dropped assignment must eventually be abandoned")
	assert.GreaterOrEqual(t, srv.RefreshSessionCalls()-refreshesBefore, 2,
		"abandoning takes at least two statistics readings, not one")
}

// TestListener_AbandonedAssignmentClearsTheStall is the operator-visible half, and the
// one a drain depends on: giving up on the assignment clears JobProvisionStalled and
// records the loss as a Warning, so a RunnerSet stops reporting work it will never do.
// Before this the condition stayed True until someone deleted the gateway by hand.
func TestListener_AbandonedAssignmentClearsTheStall(t *testing.T) {
	srv := newQuickPollServer(t)

	sink := &statusSink{}
	m := newCountingMetrics()
	_, ssID := startListenerWithSink(t, srv, sink, fastDeferredRetry, fastAssignmentCheck,
		func(c *scalesetlistener.Config) { c.Metrics = m })

	jobID := stallJob(t, srv, ssID, m, 1)
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionTrue, v2alpha1.ReasonRunnerNameConflict)
	}, 5*time.Second, 10*time.Millisecond, "the held job must be reported as a stall")

	require.True(t, srv.DropAssignedJob(ssID, jobID))

	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionFalse, v2alpha1.ReasonJobsProvisioning)
	}, 5*time.Second, 10*time.Millisecond,
		"abandoning the last held assignment must clear the stall condition")
	assert.Equal(t, 1, sink.eventCount("AssignmentAbandoned"),
		"the loss is recorded once, as its own event reason")
}

// TestListener_AbandonedAssignmentSurvivesASessionReplay closes the hole the check would
// otherwise leave open. Dropping a job from the deferred set is not enough on its own: a
// session re-create polls from cursor 0 and replays the very JobAssigned the check acted
// on, so without a record of the verdict the next replay would provision the worker the
// check exists to prevent — and a session drop is routine, not exotic.
func TestListener_AbandonedAssignmentSurvivesASessionReplay(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m,
		fastDeferredRetry, fastAssignmentCheck)
	capVal.set(5)

	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID))
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the dropped assignment must be abandoned first")

	// The name conflict clears, so nothing but the verdict stands between the replayed
	// assignment and a worker pod.
	srv.ClearJITConfigConflicts()
	srv.DropSession(ssID)
	require.Eventually(t, func() bool { return srv.HasActiveSession(ssID) }, 5*time.Second, 10*time.Millisecond,
		"the listener must re-create the session and replay the queue")

	time.Sleep(time.Second)
	assert.Empty(t, prov.jobIDs(), "a replayed assignment for an abandoned job must not provision a worker")
	assert.Zero(t, m.deferredCount(), "nor may it re-enter the deferred set")
}

// TestListener_AbandonsEveryDanglingAssignment covers the shape the rc.3 gate actually
// recorded: several assignments held at once against a scale set that counts none. All of
// them go, in one pass — a fix that cleared them one reading at a time would still leave a
// drain waiting minutes on a backlog it can never finish.
func TestListener_AbandonsEveryDanglingAssignment(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m,
		fastDeferredRetry, fastAssignmentCheck)
	capVal.set(5)

	const held = 5
	ids := make([]string, 0, held)
	for i := 0; i < held; i++ {
		ids = append(ids, stallJob(t, srv, ssID, m, i+1))
	}
	for _, id := range ids {
		require.True(t, srv.DropAssignedJob(ssID, id))
	}

	require.Eventually(t, func() bool { return m.deferredCount() == 0 }, 5*time.Second, 10*time.Millisecond,
		"every dangling assignment must be given up on")
	assert.Equal(t, held, m.abandonedCount(), "each abandoned assignment is counted")
	assert.Empty(t, prov.jobIDs(), "no worker is provisioned for any of them")
}
