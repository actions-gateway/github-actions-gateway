package scalesetlistener_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the Q661 discrimination: which identifier makes two assignments the same
// assignment. A live scale-set run left two workers carrying one run id, and the reading
// on record was that a replayed queue message had built the second. The two readings
// predict different counts — a replay carries the job's own UUID and yields one worker,
// sibling jobs of one run yield two — and nothing in that capture separated them.
//
// The run id is deliberately the shared field: it is what the eviction harness looked a
// worker up by, and one run legitimately has many jobs, so it can never name a worker.
// The stub gives every enqueued job a run of its own so that per-run bookkeeping bugs
// stay visible, which is why this shape has to be seeded.

// The identifiers the live capture recorded, extended to whole UUIDs — the capture kept
// only each one's first group, and both are jobIDs.
const (
	q661FirstJob  = "22463488-0000-4000-8000-000000000000"
	q661SecondJob = "cec0e443-0000-4000-8000-000000000000"
	q661RunID     = 30864091648
)

// seedAssignment appends a message carrying one JobAssigned for the Q661 run, so every
// assignment in these tests differs from its siblings in the jobID alone.
func seedAssignment(srv *scalesettest.Server, jobID string) {
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType:    scaleset.MessageTypeJobAssigned,
		JobID:          jobID,
		OwnerName:      "acme",
		RepositoryName: "gateway-test",
		WorkflowRunID:  q661RunID,
	}})
}

// TestListener_SiblingJobsOfOneRunEachProvisionTheirOwnWorker puts both shapes in one
// queue log: two distinct jobs of one workflow run, then a redelivery of the first job's
// assignment. Two workers must exist at the end — one per jobID, not one per run, and not
// a third for the replay.
func TestListener_SiblingJobsOfOneRunEachProvisionTheirOwnWorker(t *testing.T) {
	srv := newQuickPollServer(t)

	seedAssignment(srv, q661FirstJob)
	seedAssignment(srv, q661SecondJob)
	seedAssignment(srv, q661FirstJob) // the replay the row suspected

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	m := newCountingMetrics()
	startListener(t, srv, fixedCapacity(5), prov, m)

	require.Eventually(t, func() bool { return m.provisionedCount() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"two jobs of one run must each provision a worker")
	// The replay is the last of the three messages, so it has been delivered by the time
	// the second job's worker exists. This settles the poll that read it.
	time.Sleep(200 * time.Millisecond)

	require.Equal(t, []string{q661FirstJob, q661SecondJob}, prov.jobIDs(),
		"one worker per jobID: a redelivered assignment must not build a second worker for its job")
	assert.Equal(t, 2, m.provisionedCount(), "and the metric counts workers, not deliveries")

	wantRun := strconv.FormatInt(q661RunID, 10)
	jobs := prov.jobs()
	assert.Equal(t, wantRun, jobs[0].RunID)
	assert.Equal(t, wantRun, jobs[1].RunID,
		"both workers carry the same run id, which is why the run cannot identify either of them")
	assert.NotEqual(t, jobs[0].RunnerName, jobs[1].RunnerName,
		"and each registers its own runner, which is the identifier that does name a worker")
}

// TestListener_ReplayedAssignmentProvisionsOnceAcrossBatches is the replay on its own,
// with no sibling job in the log. It is what makes the worker count unambiguous: every
// worker here is attributable to a redelivery, so the count alone reads as the answer the
// live capture could not give.
func TestListener_ReplayedAssignmentProvisionsOnceAcrossBatches(t *testing.T) {
	srv := newQuickPollServer(t)

	const deliveries = 3
	for i := 0; i < deliveries; i++ {
		seedAssignment(srv, q661FirstJob)
	}

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	m := newCountingMetrics()
	startListener(t, srv, fixedCapacity(5), prov, m)

	require.Eventually(t, func() bool {
		assigned, _, _ := m.snapshot()
		return assigned >= deliveries
	}, 5*time.Second, 10*time.Millisecond,
		"every delivery must be read before a count of the workers they built means anything")

	assert.Equal(t, []string{q661FirstJob}, prov.jobIDs(), "three deliveries of one assignment build one worker")
	assert.Equal(t, 1, m.provisionedCount())
	assert.Zero(t, m.deferredCount(), "a deduped delivery is not a stalled one")
}
