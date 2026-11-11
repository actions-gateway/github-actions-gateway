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

// Q1327: one repository's out-of-memory kills write a floor, a parked re-run and a
// merged disruption per job, none of which the cap evicts, so its matrix runs alone
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

// TestRerunLedger_OneRepositoryCannotFillTheLedger kills every job of 256-job matrix
// runs in one repository, more than GitHub keeps in one run's matrix, and then asks for
// a re-run in a second repository.
func TestRerunLedger_OneRepositoryCannotFillTheLedger(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)

	// 564 such jobs filled the ledger before Q1327.
	const runs, jobs = 3, 256
	for r := range runs {
		runID := fmt.Sprint(30_000_000_000 + r)
		for i := range jobs {
			require.NoError(t, oomKillJob(ctx, p, target, "noisy/repo", runID, i),
				"run %d job %d: one repository's kills filled the ledger", r, i)
		}
	}
	t.Logf("ledger holds %d bytes after %d runs of %d killed jobs", len(readLedgerData(t, c, target)), runs, jobs)
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Len(t, l.Pending, runs, "one parked re-run per run attempt")
	for runID, r := range l.Reruns {
		for _, a := range r.Attempts {
			assert.LessOrEqual(t, len(a.Disruptions), maxMergedPerAttempt, "run %s lists every merged disruption", runID)
			assert.LessOrEqual(t, len(a.Jobs), maxMergedPerAttempt, "run %s lists every merged job", runID)
		}
	}

	_, _, err = p.reserveScaleSetRerun(ctx, target, "40000000000",
		rerunDisruption{key: "other-pod", job: "test", failedAttempt: 1}, recoveryCauseEviction, 2, nil)
	require.NoError(t, err, "another repository's re-run is refused")
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
