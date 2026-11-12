package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Q1327: one repository's out-of-memory kills wrote a floor, a parked re-run and a
// merged disruption per job, none of which the cap evicted, so its matrix runs alone
// could fill the per-set ledger and every other repository's re-run was then refused.

// oomKillJob records what recoverOOMKilled writes for one killed matrix job: its floor,
// then a parked reservation under its run's single FailedJobs attempt.
func oomKillJob(ctx context.Context, p *Provisioner, target Target, repo, runID string, i int) error {
	job := fmt.Sprintf("%s (%03d)", strings.Repeat("j", 94), i)
	pod := fmt.Sprintf("%s-%s-%03d", strings.Repeat("p", 47), runID, i)
	if _, err := p.recordMemoryFloor(ctx, target, runID, job, memoryFloor{
		Pod: pod, Container: "runner",
		KilledLimit: resource.MustParse("1536Mi"), Limit: resource.MustParse("2304Mi"),
	}); err != nil {
		return err
	}
	_, _, err := p.reserveScaleSetRerun(ctx, target, runID,
		rerunDisruption{key: pod, job: job, failedAttempt: 1}, recoveryCauseOOM, 2,
		&pendingRerun{Cause: recoveryCauseOOM, Repository: repo, RunID: runID, Job: job, Disruption: pod, FailedAttempt: 1})
	return err
}

// TestRerunLedger_KilledJobsOfOneAttemptAddNoBudgetState kills 40 jobs of one run
// attempt. Before Q1327 each wrote a parked re-run and a merged entry into the run's
// budget record, which is never evicted, and 564 such jobs filled the ledger; now the
// attempt keeps one parked re-run and a bounded list, so all that grows per job is its
// floor, which TestRerunLedger_FloorsAreEvictedBeforeABudgetIsRefused evicts.
func TestRerunLedger_KilledJobsOfOneAttemptAddNoBudgetState(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)

	const runID, jobs = "30000000000", 40
	for i := range jobs {
		require.NoError(t, oomKillJob(ctx, p, target, "noisy/repo", runID, i))
	}
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Len(t, l.Pending, 1, "one parked re-run per run attempt")
	assert.Len(t, l.MemoryFloors, jobs, "every killed job keeps its floor")
	a := l.Reruns[runID].Attempts["1"]
	require.NotNil(t, a)
	assert.Len(t, a.Disruptions, maxMergedPerAttempt, "the attempt lists every merged disruption")
	assert.Len(t, a.Jobs, maxMergedPerAttempt, "the attempt lists every merged job")
}

// TestRerunLedger_FloorsAreEvictedBeforeABudgetIsRefused fills the ledger past its cap
// with memory floors alone, as a week of one repository's kills can, and asks for a
// budget: the oldest floors give way rather than the re-run.
func TestRerunLedger_FloorsAreEvictedBeforeABudgetIsRefused(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	now := p.nowFn().UTC()

	l := &rerunLedger{Version: rerunLedgerVersion, MinWriterVersion: rerunLedgerMinWriterVersion, MemoryFloors: map[string]*memoryFloor{}}
	for i := range 3_000 {
		l.MemoryFloors[failureKey(fmt.Sprint(30_000_000_000+i/256), fmt.Sprintf("%s (%03d)", strings.Repeat("j", 94), i%256))] = &memoryFloor{
			ExpiresAt: now.Add(failureRecordTTL - time.Duration(3_000-i)*time.Minute),
			Pod:       fmt.Sprintf("%s-%04d", strings.Repeat("p", 58), i), Container: "runner",
			KilledLimit: resource.MustParse("1536Mi"), Limit: resource.MustParse("2304Mi"),
		}
	}
	data, err := json.Marshal(l)
	require.NoError(t, err)
	require.Greater(t, len(data), rerunLedgerMaxBytes, "the fixture is past the cap")
	writeLedger(t, c, target, string(data))

	_, _, err = p.reserveScaleSetRerun(ctx, target, "40000000000",
		rerunDisruption{key: "other-pod", job: "test", failedAttempt: 1}, recoveryCauseEviction, 2, nil)
	require.NoError(t, err, "a floor-filled ledger refuses another repository's re-run")

	got, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Contains(t, got.Reruns, "40000000000")
	assert.LessOrEqual(t, len(readLedgerData(t, c, target)), failureRecordsCeiling)
	newest := failureKey(fmt.Sprint(30_000_000_000+2_999/256), fmt.Sprintf("%s (%03d)", strings.Repeat("j", 94), 2_999%256))
	assert.Contains(t, got.MemoryFloors, newest, "the newest floor is kept")
}
